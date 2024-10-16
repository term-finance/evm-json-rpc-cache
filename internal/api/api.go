package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

	"compress/gzip"

	"github.com/allegro/bigcache/v3"
	"github.com/eko/gocache/lib/v4/cache"
	"github.com/eko/gocache/lib/v4/store"
	bigcache_store "github.com/eko/gocache/store/bigcache/v4"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/fxamacker/cbor/v2"
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
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		s.logger.Error("Error reading request body", zap.Error(err))
		http.Error(w, "Error reading request", http.StatusInternalServerError)
		return
	}
	r.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))

	var requests []map[string]interface{}
	var isBatchRequest bool

	// Attempt to unmarshal as batch request
	if err := json.Unmarshal(bodyBytes, &requests); err == nil {
		isBatchRequest = true
	} else {
		// Attempt to unmarshal as single request
		var singleRequest map[string]interface{}
		if err := json.Unmarshal(bodyBytes, &singleRequest); err != nil {
			s.logger.Error("Error unmarshaling request body", zap.Error(err))
			http.Error(w, "Invalid request", http.StatusBadRequest)
			return
		}
		requests = []map[string]interface{}{singleRequest}
		isBatchRequest = false
	}

	responses := make([]interface{}, len(requests))
	uncachedRequests := []map[string]interface{}{}
	uncachedRequestIDs := []interface{}{}
	idToIndex := make(map[interface{}]int)
	uncachedRequestsByID := make(map[interface{}]map[string]interface{})

	for i, req := range requests {
		id, ok := req["id"]
		if !ok {
			s.logger.Error("Missing 'id' in request")
			http.Error(w, "Invalid request: missing 'id'", http.StatusBadRequest)
			return
		}
		idToIndex[id] = i

		method, ok := req["method"].(string)
		if !ok {
			s.logger.Error("Invalid method in request")
			http.Error(w, "Invalid request", http.StatusBadRequest)
			return
		}
		params := req["params"]

		cacheKey := fmt.Sprintf("%s:%s", method, hashParams(params))
		shouldCache, _ := s.shouldCacheEndpoint(method)

		if shouldCache {
			cachedResponseBytes, err := s.Cache.Get(r.Context(), cacheKey)
			if err == nil {
				// Cache hit
				var cachedResponse map[string]interface{}

				// Manually decode CBOR into map[string]interface{}
				if err := cbor.Unmarshal(cachedResponseBytes, &cachedResponse); err == nil {
					// Inject the current request's 'id' into the cached response
					cachedResponse["id"] = id
					responses[i] = cachedResponse
					continue
				} else {
					s.logger.Error("Error unmarshaling cached CBOR response", zap.Error(err))
				}
			}
		}

		uncachedRequests = append(uncachedRequests, req)
		uncachedRequestIDs = append(uncachedRequestIDs, id)
		uncachedRequestsByID[id] = req
	}

	if len(uncachedRequests) > 0 {
		// Send uncached requests to backend
		backendBodyBytes, err := json.Marshal(uncachedRequests)
		if err != nil {
			s.logger.Error("Error marshaling uncached requests", zap.Error(err))
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		// Create a new request to send to the backend
		backendReq, err := http.NewRequest("POST", s.Config.Backend.URL, bytes.NewBuffer(backendBodyBytes))
		if err != nil {
			s.logger.Error("Error creating backend request", zap.Error(err))
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		backendReq.Header = r.Header.Clone()

		// Send the request to the backend
		client := &http.Client{}
		resp, err := client.Do(backendReq)
		if err != nil {
			s.logger.Error("Error sending request to backend", zap.Error(err))
			http.Error(w, "Error connecting to backend", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		// Read and process the backend response
		responseBody, err := io.ReadAll(resp.Body)
		if err != nil {
			s.logger.Error("Error reading backend response", zap.Error(err))
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		if resp.Header.Get("Content-Encoding") == "gzip" {
			gzipReader, err := gzip.NewReader(bytes.NewReader(responseBody))
			if err != nil {
				s.logger.Error("Error creating gzip reader", zap.Error(err))
				http.Error(w, "Error decompressing response", http.StatusInternalServerError)
				return
			}
			decompressedBody, err := io.ReadAll(gzipReader)
			if err != nil {
				s.logger.Error("Error reading decompressed data", zap.Error(err))
				http.Error(w, "Error decompressing response", http.StatusInternalServerError)
				return
			}
			gzipReader.Close()
			responseBody = decompressedBody
		}

		var backendResponses []interface{}
		if err := json.Unmarshal(responseBody, &backendResponses); err != nil {
			// Handle single response
			var singleResponse interface{}
			if err := json.Unmarshal(responseBody, &singleResponse); err != nil {
				s.logger.Error("Error unmarshaling backend response", zap.Error(err))
				http.Error(w, "Internal server error", http.StatusInternalServerError)
				return
			}
			backendResponses = []interface{}{singleResponse}
		}

		// Map backend responses by 'id'
		backendResponsesByID := make(map[interface{}]interface{})
		for _, backendResp := range backendResponses {
			respMap, ok := backendResp.(map[string]interface{})
			if !ok {
				s.logger.Error("Invalid response format")
				http.Error(w, "Internal server error", http.StatusInternalServerError)
				return
			}
			id, ok := respMap["id"]
			if !ok {
				s.logger.Error("Missing 'id' in backend response")
				http.Error(w, "Invalid backend response: missing 'id'", http.StatusInternalServerError)
				return
			}
			backendResponsesByID[id] = backendResp
		}

		// Place backend responses into the correct positions
		for _, id := range uncachedRequestIDs {
			index := idToIndex[id]
			backendResp, ok := backendResponsesByID[id]
			if !ok {
				s.logger.Error("Received response with unknown id", zap.Any("id", id))
				http.Error(w, "Invalid backend response: unknown 'id'", http.StatusInternalServerError)
				return
			}
			responses[index] = backendResp

			req := uncachedRequestsByID[id]

			method := req["method"].(string)
			params := req["params"]
			cacheKey := fmt.Sprintf("%s:%s", method, hashParams(params))
			shouldCache, cacheTTL := s.shouldCacheEndpoint(method)

			if shouldCache {
				// Remove 'id' from the response before caching
				respMap, ok := backendResp.(map[string]interface{})
				if !ok {
					s.logger.Error("Invalid response format")
					continue
				}
				// Make a copy of the response without 'id'
				cachedResp := make(map[string]interface{})
				for k, v := range respMap {
					if k != "id" {
						cachedResp[k] = v
					}
				}
				// Marshal the response without 'id'
				cachedResponseBytes, err := cbor.Marshal(cachedResp)
				if err == nil {
					if err := s.Cache.Set(r.Context(), cacheKey, cachedResponseBytes, store.WithExpiration(cacheTTL)); err != nil {
						s.logger.Error("Error setting cache for key", zap.String("key", cacheKey), zap.Error(err))
					}
				} else {
					s.logger.Error("Error marshaling response for caching", zap.Error(err))
				}
			}
		}
	}

	// Before marshaling the responses, convert them to have string keys
	for i, resp := range responses {
		responses[i] = convertToStringKeys(resp)
	}

	// Assemble and send the batch response
	var responseBytes []byte
	if isBatchRequest {
		responseBytes, err = json.Marshal(responses)
	} else {
		responseBytes, err = json.Marshal(responses[0])
	}

	if err != nil {
		s.logger.Error("Error marshaling response", zap.Error(err))
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(responseBytes)
}

func hashParams(params interface{}) string {
	// Convert params to a sorted JSON string to ensure consistent hashing
	paramsJSON, _ := json.Marshal(params)
	h := sha256.New()
	h.Write(paramsJSON)
	return hex.EncodeToString(h.Sum(nil))
}

type responseRecorder struct {
	statusCode int
	header     http.Header
	body       *bytes.Buffer
}

func (r *responseRecorder) Header() http.Header {
	if r.header == nil {
		r.header = make(http.Header)
	}
	return r.header
}

func (r *responseRecorder) WriteHeader(code int) {
	if r.statusCode == 0 {
		r.statusCode = code
	}
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	return r.body.Write(b)
}

func NewResponseRecorder() *responseRecorder {
	return &responseRecorder{
		header:     make(http.Header),
		body:       &bytes.Buffer{},
		statusCode: http.StatusOK,
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

func (s *Server) shouldCacheEndpoint(method string) (bool, time.Duration) {
	switch method {
	case "eth_getTransactionByHash", "eth_getTransactionReceipt",
		"eth_getTransactionByBlockHashAndIndex", "eth_getTransactionByBlockNumberAndIndex":
		return true, 365 * 24 * time.Hour
	case "eth_estimateGas", "eth_maxPriorityFeePerGas", "eth_gasPrice":
		return true, s.Config.Blockchain.GasFeeTTL
	case "eth_chainId":
		return true, 365 * 24 * time.Hour
	case "eth_blockNumber", "eth_getBalance", "eth_getTransactionCount", "eth_sendRawTransaction":
		return false, 0
	case "eth_getBlockByHash", "eth_getBlockByNumber",
		"eth_getBlockTransactionCountByHash", "eth_getBlockTransactionCountByNumber":
		return true, 365 * 24 * time.Hour
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

func convertToStringKeys(i interface{}) interface{} {
	switch v := i.(type) {
	case map[interface{}]interface{}:
		return convertMap(v)
	case []interface{}:
		return convertSlice(v)
	case map[string]interface{}:
		// Also need to process nested structures
		for key, val := range v {
			v[key] = convertToStringKeys(val)
		}
		return v
	default:
		return v
	}
}

func convertMap(m map[interface{}]interface{}) map[string]interface{} {
	result := make(map[string]interface{})
	for k, v := range m {
		keyStr := fmt.Sprintf("%v", k)
		result[keyStr] = convertToStringKeys(v)
	}
	return result
}

func convertSlice(s []interface{}) []interface{} {
	result := make([]interface{}, len(s))
	for i, v := range s {
		result[i] = convertToStringKeys(v)
	}
	return result
}
