# EVM JSON RPC Cache

This program caches requests 

## Caching Mechanism

1. **Cache Storage**: The server uses BigCache as the underlying cache storage mechanism, which is wrapped by the `gocache` library for easier management.

2. **Cache Key**: For each request, a unique cache key is generated based on the HTTP method and the full URL of the request.

3. **Caching Decision**: The `shouldCacheEndpoint` method determines whether a request should be cached and for how long, based on the Ethereum JSON-RPC method being called.

4. **Cached Response**: When a response is cached, it stores the entire HTTP response, including status code, headers, and body.

5. **Cache Hit**: If a cached response is found, it's immediately returned to the client without forwarding the request to the backend.

6. **Cache Miss**: If no cached response is found, the request is forwarded to the backend, and the response is then cached (if the method is cacheable).

7. **Confirmation-based Caching**: For transaction and block-related methods, the caching decision also considers the number of confirmations:
   - Transaction data is cached only if the transaction has reached the required number of confirmations.
   - Block data is cached for a long time if the block is considered finalized (has enough confirmations).

8. **Dynamic TTL**: Different cache TTLs (Time To Live) are set for different types of data:
   - Confirmed transaction and finalized block data: 365 days (some long time)
   - Gas price estimates: based on config

9. **Latest Block Tracking**: The server continuously tracks tip through wss to determine finality

# Setup 


```bash
make
```

```bash
./proxy -config ./config.yml
```

## docker
```bash
docker compose up -d
```

# config

here is an example config

```yaml
server:
  listenAddress: "0.0.0.0"
  listenPort: 8080

backend:
  url: "https://mainnet.infura.io/v3/API_KEY"

ws_backend:
  url: "wss://mainnet.infura.io/ws/v3/API_KEY"

blockchain:
  confirmations: 15
  gasFeeTTL: 30s

logging:
  level: "info"
  logDirectory: "logs"
  logFileName: "app.log"
  maxLogFileSize: 100
  maxBackups: 3
  maxAge: 28

cache:
  shards: 1024
  lifeWindow: 10m
  cleanWindow: 5m
  maxEntriesInWindow: 600000
  maxEntrySize: 500
  verbose: false
  hardMaxCacheSize: 8192
```