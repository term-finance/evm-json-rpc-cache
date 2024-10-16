package api

import (
	"bytes"
	"context"
	"encoding/json"
	"evm-cache/internal/config"
	"evm-cache/internal/logging"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"math/big"

	"github.com/allegro/bigcache/v3"
	"github.com/eko/gocache/lib/v4/cache"
	"github.com/eko/gocache/lib/v4/store"
	bigcache_store "github.com/eko/gocache/store/bigcache/v4"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"go.uber.org/zap"
)

type CachedResponse struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

type Server struct {
	Config         *config.Config
	Proxy          *httputil.ReverseProxy
	ListenAddr     string
	Cache          *cache.Cache[[]byte]
	latestBlockNum atomic.Uint64
	ethClient      *ethclient.Client
	logger         *zap.SugaredLogger
}

func NewServer(cfg *config.Config) (*Server, error) {
	logger := logging.GetLogger()

	backendURL, err := url.Parse(cfg.Backend.URL)
	if err != nil {
		return nil, fmt.Errorf("invalid backend URL %s: %v", cfg.Backend.URL, err)
	}

	// For infura Extract the project ID from the path
	projectID := backendURL.Path[1:]
	backendURL.Path = ""

	proxy := httputil.NewSingleHostReverseProxy(backendURL)

	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.Host = backendURL.Host
		req.URL.Path = fmt.Sprintf("/%s", projectID)
	}

	proxy.Transport = &customTransport{
		originalTransport: http.DefaultTransport,
		logger:            logger,
	}

	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, e error) {
		logger.Error("Proxy error", zap.Error(e))

		var statusCode int
		var errorMessage string

		switch {
		case strings.Contains(e.Error(), "dial tcp"):
			statusCode = http.StatusBadGateway
			errorMessage = "Unable to connect to the backend service"
		case strings.Contains(e.Error(), "timeout"):
			statusCode = http.StatusGatewayTimeout
			errorMessage = "Backend service timed out"
		default:
			statusCode = http.StatusInternalServerError
			errorMessage = "An unexpected error occurred"
		}

		http.Error(w, errorMessage, statusCode)
	}

	serverAddr := fmt.Sprintf("%s:%d", cfg.Server.ListenAddress, cfg.Server.ListenPort)

	bigCacheConfig := bigcache.DefaultConfig(cfg.Cache.LifeWindow)
	bigCacheConfig.Shards = cfg.Cache.Shards
	bigCacheConfig.CleanWindow = cfg.Cache.CleanWindow
	bigCacheConfig.MaxEntriesInWindow = cfg.Cache.MaxEntriesInWindow
	bigCacheConfig.MaxEntrySize = cfg.Cache.MaxEntrySize
	bigCacheConfig.Verbose = cfg.Cache.Verbose
	bigCacheConfig.HardMaxCacheSize = cfg.Cache.HardMaxCacheSize

	bigCache, err := bigcache.New(context.Background(), bigCacheConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize BigCache: %v", err)
	}

	bigcacheStore := bigcache_store.NewBigcache(bigCache)
	cacheManager := cache.New[[]byte](bigcacheStore)

	ethClient, err := ethclient.Dial(cfg.WSBackend.URL)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to Ethereum client: %v", err)
	}

	server := &Server{
		Config:     cfg,
		Proxy:      proxy,
		ListenAddr: serverAddr,
		Cache:      cacheManager,
		ethClient:  ethClient,
		logger:     logger,
	}

	// Start tracking the latest block
	go server.trackLatestBlock()

	return server, nil
}

func (s *Server) trackLatestBlock() {
	for {
		if err := s.subscribeToBlocks(); err != nil {
			s.logger.Error("Block subscription failed. Retrying in 5 seconds...", zap.Error(err))
			time.Sleep(5 * time.Second)
		}
	}
}

func (s *Server) subscribeToBlocks() error {
	headers := make(chan *types.Header)
	sub, err := s.ethClient.SubscribeNewHead(context.Background(), headers)
	if err != nil {
		return fmt.Errorf("failed to subscribe to new headers: %v", err)
	}
	defer sub.Unsubscribe()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case err := <-sub.Err():
			return fmt.Errorf("subscription error: %v", err)
		case header := <-headers:
			s.latestBlockNum.Store(header.Number.Uint64())
			s.logger.Infow("New block",
				"number", header.Number.Uint64(),
			)
		case <-ticker.C:
			block, err := s.ethClient.BlockByNumber(context.Background(), nil)
			if err != nil {
				s.logger.Error("Failed to fetch latest block", zap.Error(err))
			} else {
				s.latestBlockNum.Store(block.NumberU64())
				s.logger.Infow("Latest block (fallback)",
					"number", block.NumberU64(),
				)
			}
		}
	}
}

func (s *Server) Start() {
	http.HandleFunc("/", s.loggingMiddleware(s.handleProxy))

	s.logger.Infow("Starting proxy server",
		"address", s.ListenAddr,
		"backend", s.Config.Backend.URL,
	)
	if err := http.ListenAndServe(s.ListenAddr, nil); err != nil {
		s.logger.Fatal("Failed to start server", zap.Error(err))
	}
}

// handleProxy forwards the incoming request to the backend using ReverseProxy with caching
func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	// Generate a unique cache key based on the request
	cacheKey := fmt.Sprintf("%s:%s", r.Method, r.URL.String())

	shouldCache, cacheTTL := s.shouldCacheEndpoint(r)

	if shouldCache {
		// Attempt to retrieve the response from cache
		cachedResponseBytes, err := s.Cache.Get(r.Context(), cacheKey)
		if err == nil {
			// cache hit
			var cachedResponse CachedResponse
			if err := json.Unmarshal(cachedResponseBytes, &cachedResponse); err != nil {
				s.logger.Error("Error unmarshaling cached response", zap.Error(err))
			} else {
				for key, values := range cachedResponse.Header {
					for _, value := range values {
						w.Header().Add(key, value)
					}
				}
				w.WriteHeader(cachedResponse.StatusCode)
				_, writeErr := w.Write(cachedResponse.Body)
				if writeErr != nil {
					s.logger.Error("Error writing cached response", zap.Error(writeErr))
				}
				return
			}
		}
	}

	// cache miss
	rec := NewResponseRecorder(w)

	s.Proxy.ServeHTTP(rec, r)

	if shouldCache {

		newCachedResponse := &CachedResponse{
			StatusCode: rec.statusCode,
			Header:     rec.header.Clone(),
			Body:       rec.body.Bytes(),
		}

		serializedResponse, err := json.Marshal(newCachedResponse)
		if err != nil {
			s.logger.Error("Error serializing response for caching", zap.Error(err))
		} else {
			if err := s.Cache.Set(r.Context(), cacheKey, serializedResponse, store.WithExpiration(cacheTTL)); err != nil {
				s.logger.Error("Error setting cache for key", zap.String("key", cacheKey), zap.Error(err))
			}
		}
	}
}

type responseRecorder struct {
	http.ResponseWriter
	statusCode int
	header     http.Header
	body       *bytes.Buffer
}

func (r *responseRecorder) WriteHeader(code int) {
	r.statusCode = code
	r.header = r.Header().Clone()
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	r.body.Write(b)
	return r.ResponseWriter.Write(b)
}

func NewResponseRecorder(w http.ResponseWriter) *responseRecorder {
	return &responseRecorder{
		ResponseWriter: w,
		header:         make(http.Header),
		body:           &bytes.Buffer{},
		statusCode:     http.StatusOK,
	}
}

type customTransport struct {
	originalTransport http.RoundTripper
	logger            *zap.SugaredLogger
}

func (t *customTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.originalTransport.RoundTrip(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewBuffer(body))
		t.logger.Error("Backend error", zap.Int("status", resp.StatusCode), zap.String("body", string(body)))
	}

	return resp, nil
}

func (s *Server) loggingMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		s.logger.Infow("Started request",
			"method", r.Method,
			"path", r.URL.Path,
		)

		next(w, r)

		duration := time.Since(start)
		s.logger.Infow("Completed request",
			"method", r.Method,
			"path", r.URL.Path,
			"duration_ms", float64(duration.Nanoseconds())/float64(time.Millisecond),
		)
	}
}

func (s *Server) shouldCacheEndpoint(r *http.Request) (bool, time.Duration) {
	// Extract the method from the request body
	var requestBody struct {
		Method string `json:"method"`
	}

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		s.logger.Error("Error reading request body", zap.Error(err))
		return false, 0
	}

	// Restore the request body for further processing
	r.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))

	err = json.Unmarshal(bodyBytes, &requestBody)
	if err != nil {
		s.logger.Error("Error unmarshaling request body", zap.Error(err))
		return false, 0
	}

	method := requestBody.Method

	s.logger.Infow("Method called", "method", method)

	switch method {

	case "eth_getTransactionByHash", "eth_getTransactionReceipt",
		"eth_getTransactionByBlockHashAndIndex", "eth_getTransactionByBlockNumberAndIndex":
		// For any tx data, it should be cached if it's over the required confirmations
		if s.isTransactionConfirmed(r) {
			return true, 365 * 24 * time.Hour
		}
		return false, 0

	case "eth_estimateGas", "eth_maxPriorityFeePerGas", "eth_gasPrice":
		return true, s.Config.Blockchain.GasFeeTTL

	case "eth_chainId":
		return true, 365 * 24 * time.Hour

	case "eth_blockNumber", "eth_getBalance",
		"eth_getTransactionCount", "eth_sendRawTransaction":
		return false, 0

	case "eth_getBlockByHash", "eth_getBlockByNumber",
		"eth_getBlockTransactionCountByHash", "eth_getBlockTransactionCountByNumber":
		// cache for a long time if it's over 15 confirmations
		if s.isBlockFinalized(r) {
			return true, 365 * 24 * time.Hour
		}
		return false, 0

	default:
		return false, 0
	}
}

func (s *Server) isBlockFinalized(r *http.Request) bool {
	var requestBody struct {
		Params []interface{} `json:"params"`
	}

	bodyBytes, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))

	if err := json.Unmarshal(bodyBytes, &requestBody); err != nil {
		s.logger.Error("Error unmarshaling request body", zap.Error(err))
		return false
	}

	if len(requestBody.Params) == 0 {
		return false
	}

	var blockNum uint64
	var err error

	switch r.Method {
	case "eth_getBlockByHash", "eth_getBlockTransactionCountByHash":
		// fetch the block first
		blockHash := common.HexToHash(requestBody.Params[0].(string))
		block, err := s.ethClient.BlockByHash(context.Background(), blockHash)
		if err != nil {
			s.logger.Error("Error fetching block by hash", zap.Error(err))
			return false
		}
		blockNum = block.NumberU64()
	case "eth_getBlockByNumber", "eth_getBlockTransactionCountByNumber":
		blockParam := requestBody.Params[0].(string)
		if blockParam == "latest" || blockParam == "pending" || blockParam == "earliest" {
			// doesnt make sense to catch
			// TODO: figure out what earliest is
			return false
		}
		blockNum, err = strconv.ParseUint(blockParam[2:], 16, 64)
		if err != nil {
			s.logger.Error("Error parsing block number", zap.Error(err))
			return false
		}
	}

	latestBlock := s.latestBlockNum.Load()
	return latestBlock-blockNum >= uint64(s.Config.Blockchain.Confirmations)
}

// isTransactionConfirmed checks if a transaction is confirmed
func (s *Server) isTransactionConfirmed(r *http.Request) bool {
	var requestBody struct {
		Params []interface{} `json:"params"`
	}

	bodyBytes, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))

	if err := json.Unmarshal(bodyBytes, &requestBody); err != nil {
		s.logger.Error("Error unmarshaling request body", zap.Error(err))
		return false
	}

	if len(requestBody.Params) == 0 {
		return false
	}

	var txHash string
	switch r.Method {
	case "eth_getTransactionByHash", "eth_getTransactionReceipt":
		txHash, _ = requestBody.Params[0].(string)
	case "eth_getTransactionByBlockHashAndIndex", "eth_getTransactionByBlockNumberAndIndex":
		// For these methods, we need to fetch the transaction first
		blockParam := requestBody.Params[0].(string)
		indexParam := requestBody.Params[1].(string)

		var block *types.Block
		var err error

		if strings.HasPrefix(blockParam, "0x") {
			blockHash := common.HexToHash(blockParam)
			block, err = s.ethClient.BlockByHash(context.Background(), blockHash)
		} else {
			blockNumber := new(big.Int)
			blockNumber.SetString(blockParam[2:], 16)
			block, err = s.ethClient.BlockByNumber(context.Background(), blockNumber)
		}

		if err != nil {
			s.logger.Error("Error fetching block", zap.Error(err))
			return false
		}

		index, _ := strconv.ParseUint(indexParam[2:], 16, 64)
		if int(index) >= len(block.Transactions()) {
			s.logger.Error("Transaction index out of range")
			return false
		}

		txHash = block.Transactions()[index].Hash().Hex()
	}

	tx, _, err := s.ethClient.TransactionByHash(context.Background(), common.HexToHash(txHash))
	if err != nil {
		s.logger.Error("Error fetching transaction", zap.Error(err))
		return false
	}

	receipt, err := s.ethClient.TransactionReceipt(context.Background(), tx.Hash())
	if err != nil {
		s.logger.Error("Error fetching transaction receipt", zap.Error(err))
		return false
	}

	latestBlock := s.latestBlockNum.Load()
	return latestBlock-receipt.BlockNumber.Uint64() >= uint64(s.Config.Blockchain.Confirmations)
}
