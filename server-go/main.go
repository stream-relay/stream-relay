package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"expvar"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/redis/go-redis/v9"
)

/*
Environment (parity with Node):
PORT=29999
REDIS_URL=redis://127.0.0.1:6379
TTL_SECONDS=1800
CONFIG_PATH=../config.json
LONG_POLL_MAX_MS=30000
LONG_POLL_POLL_INTERVAL_MS=100

CORS_ALLOW_ORIGINS="*" or "https://app.example.com,https://admin.example.com"
CORS_ALLOW_CREDENTIALS="false|true"
CORS_MAX_AGE=600
CORS_ALLOW_PRIVATE_NETWORK="false|true"
*/

// ErrorLog stores recent critical errors for debugging
type ErrorLog struct {
	mu      sync.RWMutex
	entries []ErrorEntry
	maxSize int
}

type ErrorEntry struct {
	Time    time.Time `json:"time"`
	Level   string    `json:"level"`
	Source  string    `json:"source"`
	Message string    `json:"message"`
	Details string    `json:"details,omitempty"`
}

var errorLog = &ErrorLog{maxSize: 100}

func (l *ErrorLog) Add(level, source, message, details string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry := ErrorEntry{
		Time:    time.Now(),
		Level:   level,
		Source:  source,
		Message: message,
		Details: details,
	}
	l.entries = append(l.entries, entry)
	if len(l.entries) > l.maxSize {
		l.entries = l.entries[len(l.entries)-l.maxSize:]
	}
}

func (l *ErrorLog) Recent(n int) []ErrorEntry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if n <= 0 || n > len(l.entries) {
		n = len(l.entries)
	}
	// Return most recent n entries (newest first)
	result := make([]ErrorEntry, n)
	for i := 0; i < n; i++ {
		result[i] = l.entries[len(l.entries)-1-i]
	}
	return result
}

func logError(source, message, details string) {
	errorLog.Add("error", source, message, details)
	log.Printf("[%s] %s: %s", source, message, details)
}

func logWarn(source, message, details string) {
	errorLog.Add("warn", source, message, details)
	log.Printf("[%s] %s: %s", source, message, details)
}

type Config struct {
	AllowedDomains []string          `json:"allowedDomains"`
	DomainMap      map[string]string `json:"domainMap"`
}

var (
	port              = getenv("PORT", "29999")
	redisURL          = getenv("REDIS_URL", "redis://127.0.0.1:6379")
	ttlSeconds        = getenvInt("TTL_SECONDS", 1800)
	cfgPath           = getenv("CONFIG_PATH", filepath.Join(".", "config.json"))
	longPollMaxMs     = clampPosInt(getenvInt("LONG_POLL_MAX_MS", 30000), 10, 10*60*1000)
	longPollPollMs    = clampPosInt(getenvInt("LONG_POLL_POLL_INTERVAL_MS", 100), 10, longPollMaxMs)
	corsAllowOrigins  = parseCSV(getenv("CORS_ALLOW_ORIGINS", "*"))
	corsMaxAge        = getenvInt("CORS_MAX_AGE", 600)
	corsAllowCreds    = strings.EqualFold(getenv("CORS_ALLOW_CREDENTIALS", "false"), "true")
    corsAllowPrivNet    = strings.EqualFold(getenv("CORS_ALLOW_PRIVATE_NETWORK", "false"), "true")
    upstreamChunkSize   = 32 * 1024 // aggregate upstream reads into ~32 KiB before RPUSH
    pipeOutboxMaxFrames = clampPosInt(getenvInt("PIPE_OUTBOX_MAX_FRAMES", 4096), 256, 65536)
)
var (
	config     = mustLoadConfig(cfgPath)
	allowSet   = mergeAllowedDomains(config.AllowedDomains, getenv("ALLOWED_DOMAINS", ""))
	domainMap  = normalizeMapLower(config.DomainMap)
		redisCli   *redis.Client
		redisOnce  sync.Once
		httpClient *http.Client
	)
	
	func main() {
	initRedis()
	initHTTPClient()

	// Start the pub/sub notification hub for efficient long-polling
	pipehub.Start()
	// Start the cancel hub for stream cancellations (shared connection, not per-stream)
	cancelhub.Start()

	mux := http.NewServeMux()
		// Expose basic metrics via expvar
		mux.Handle("/debug/vars", expvar.Handler())
		// Expose recent error logs for debugging
		mux.HandleFunc("/debug/errors", handleDebugErrors)
		// CORS wraps everything (for Browser SDK)
		handler := withRecover(withMetrics(withCORS(mux)))
	
		// Paired long-poll bidirectional pipe endpoints
		mux.HandleFunc("/pipe/open", handlePipeOpen)
		mux.HandleFunc("/pipe/recv", handlePipeRecv)
		mux.HandleFunc("/pipe/send", handlePipeSend)
	
		// No in-memory sessions; distributed pipe uses Redis. No reaper needed.
	
		srv := &http.Server{
			Addr:         ":" + port,
			Handler:      handler,
			ReadTimeout:  300 * time.Second,
			WriteTimeout: 0, // do not cap; long responses are chunked/streamed
			IdleTimeout:  120 * time.Second,
		}
	
		log.Printf("[server] stream-relay listening on :%s", port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}
	
	
/* -------------------- Redis helpers -------------------- */

func initRedis() {
	redisOnce.Do(func() {
		opt, err := redis.ParseURL(redisURL)
		if err != nil {
			log.Fatalf("redis parse: %v", err)
		}
		// Ensure minimum pool size to prevent connection starvation.
		// The pipe protocol uses blocking XREAD for recv (holds connection for up to 30s)
		// while send operations need connections for BodyDict chunk fetches.
		// Default pool size is 10*GOMAXPROCS which can be too small on constrained systems.
		const minPoolSize = 20
		if opt.PoolSize < minPoolSize {
			opt.PoolSize = minPoolSize
		}
		redisCli = redis.NewClient(opt)
		if err := redisCli.Ping(context.Background()).Err(); err != nil {
			log.Fatalf("redis ping: %v", err)
		}
		log.Printf("[redis] connected (pool_size=%d)", opt.PoolSize)
	})
}

func rdbHGetAll(ctx context.Context, key string) (map[string]string, error) {
	return redisCli.HGetAll(ctx, key).Result()
}

func hsetMeta(ctx context.Context, key string, m map[string]string) error {
	return redisCli.HSet(ctx, key, m).Err()
}

func expireAll(ctx context.Context, keys []string) {
	pipe := redisCli.Pipeline()
	for _, k := range keys {
		pipe.Expire(ctx, k, time.Duration(ttlSeconds)*time.Second)
	}
	_, _ = pipe.Exec(ctx)
}

/* -------------------- HTTP client (upstream) -------------------- */

func initHTTPClient() {
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 60 * time.Second,
	}
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 300 * time.Second, // Timeout if upstream doesn't send headers
		MaxIdleConns:          4096,
		MaxIdleConnsPerHost:   1024,
		MaxConnsPerHost:       0, // unlimited
		IdleConnTimeout:       90 * time.Second,
		DisableCompression:    true, // do NOT auto-unzip; keep identity
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
	httpClient = &http.Client{
		Transport: tr,
		Timeout:   0, // streaming; no overall timeout
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// Do not follow; mirror Node's 'redirect: manual'
			return http.ErrUseLastResponse
		},
	}
}

/* -------------------- CORS middleware -------------------- */

func withCORS(next http.Handler) http.Handler {
	allowAll := len(corsAllowOrigins) == 1 && corsAllowOrigins[0] == "*"
	allowed := toSetExact(corsAllowOrigins)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		acao := ""
		if allowAll {
			// When credentials are enabled, we can't use "*" - must echo origin
			if corsAllowCreds && origin != "" {
				acao = origin
				w.Header().Add("Vary", "Origin")
			} else {
				acao = "*"
			}
		} else if origin != "" && allowed[origin] {
			acao = origin
			w.Header().Add("Vary", "Origin")
		}
		if acao != "" {
			w.Header().Set("Access-Control-Allow-Origin", acao)
		}
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers",
			"Content-Type, X-Stream-Relay-Target, X-Stream-Relay-Method, X-Stream-Relay-Headers")
		w.Header().Set("Access-Control-Expose-Headers",
			"X-Next-Offset, X-Done, X-Origin-Status, X-Origin-Content-Type, X-Origin-Error")
		if corsMaxAge > 0 {
			w.Header().Set("Access-Control-Max-Age", fmt.Sprintf("%d", corsMaxAge))
		}
		if corsAllowCreds {
			w.Header().Set("Access-Control-Allow-Credentials", "true")
		}
		// Chrome private network access for http://127.0.0.1 in dev
		if corsAllowPrivNet && strings.EqualFold(r.Header.Get("Access-Control-Request-Private-Network"), "true") {
			w.Header().Set("Access-Control-Allow-Private-Network", "true")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

/* -------------------- Utilities -------------------- */

func applyDomainTranslation(u *url.URL) {
	if u == nil {
		return
	}
	hostKey := strings.ToLower(u.Hostname())
	mapped, ok := domainMap[hostKey]
	if !ok || strings.TrimSpace(mapped) == "" {
		return
	}
	m := strings.TrimSpace(mapped)
	if strings.Contains(m, "://") {
		// full URL
		mu, err := url.Parse(m)
		if err != nil {
			log.Printf("[config] invalid domainMap value for %s: %s", hostKey, m)
			return
		}
		u.Scheme = mu.Scheme
		u.Host = mu.Host
		return
	}
	// host[:port]
	if strings.Contains(m, ":") {
		u.Host = m
	} else {
		// replace hostname, keep existing port if any
		port := u.Port()
		if port != "" {
			u.Host = m + ":" + port
		} else {
			u.Host = m
		}
	}
}

func readMaybeBrotli(r *http.Request) ([]byte, error) {
	decompressed, _, err := readMaybeBrotliWithSize(r)
	return decompressed, err
}

// Size limits for request bodies
const (
	maxWireBytes        = 50 * 1024 * 1024  // 50MB max wire (compressed) size
	maxDecompressedBytes = 100 * 1024 * 1024 // 100MB max decompressed size
)

// readMaybeBrotliWithSize reads the request body, decompresses if needed,
// and returns (decompressed data, wire bytes count, error)
func readMaybeBrotliWithSize(r *http.Request) ([]byte, int64, error) {
	enc := strings.ToLower(r.Header.Get("Content-Encoding"))
	defer r.Body.Close()

	// Read raw body first to get wire bytes (with limit)
	rawBody, err := io.ReadAll(io.LimitReader(r.Body, maxWireBytes+1))
	if err != nil {
		return nil, 0, err
	}
	if int64(len(rawBody)) > maxWireBytes {
		return nil, 0, fmt.Errorf("request body too large: %d bytes (max %d)", len(rawBody), maxWireBytes)
	}
	wireBytes := int64(len(rawBody))

	// Decompress if needed
	switch enc {
	case "br":
		reader := brotli.NewReader(bytes.NewReader(rawBody))
		decompressed, err := io.ReadAll(io.LimitReader(reader, maxDecompressedBytes+1))
		if err != nil {
			return nil, wireBytes, err
		}
		if int64(len(decompressed)) > maxDecompressedBytes {
			return nil, wireBytes, fmt.Errorf("decompressed body too large: %d bytes (max %d)", len(decompressed), maxDecompressedBytes)
		}
		return decompressed, wireBytes, nil
	case "gzip":
		gr, err := gzip.NewReader(bytes.NewReader(rawBody))
		if err != nil {
			return nil, wireBytes, err
		}
		defer gr.Close()
		decompressed, err := io.ReadAll(io.LimitReader(gr, maxDecompressedBytes+1))
		if err != nil {
			return nil, wireBytes, err
		}
		if int64(len(decompressed)) > maxDecompressedBytes {
			return nil, wireBytes, fmt.Errorf("decompressed body too large: %d bytes (max %d)", len(decompressed), maxDecompressedBytes)
		}
		return decompressed, wireBytes, nil
	default:
		// identity - no compression
		return rawBody, wireBytes, nil
	}
}

func decodeJSONMaybeBrotli(r *http.Request, dst any) error {
	b, err := readMaybeBrotli(r)
	if err != nil {
		return err
	}
	if len(b) == 0 {
		return io.EOF
	}
	return json.Unmarshal(b, dst)
}

// handleDebugErrors returns recent critical errors for debugging
func handleDebugErrors(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	n := 50 // default
	if q := r.URL.Query().Get("n"); q != "" {
		if parsed, err := strconvAtoi(q); err == nil && parsed > 0 {
			n = parsed
		}
	}
	entries := errorLog.Recent(n)
	writeJSON(w, http.StatusOK, map[string]any{
		"count":   len(entries),
		"entries": entries,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func getenv(key, def string) string {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func getenvInt(key string, def int) int {
	v := getenv(key, "")
	if v == "" {
		return def
	}
	n, err := strconvAtoi(v)
	if err != nil {
		return def
	}
	return n
}

func strconvAtoi(s string) (int, error) {
	var n int64
	var sign int64 = 1
	i := 0
	if len(s) > 0 && (s[0] == '-' || s[0] == '+') {
		if s[0] == '-' {
			sign = -1
		}
		i++
	}
	for ; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("invalid int")
		}
		n = n*10 + int64(c-'0')
		if n*sign > math.MaxInt || n*sign < math.MinInt {
			return 0, fmt.Errorf("overflow")
		}
	}
	return int(n * sign), nil
}

func parseCSV(s string) []string {
	parts := strings.Split(s, ",")
	var out []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return []string{"*"}
	}
	return out
}

func toSetLower(xs []string) map[string]bool {
	if len(xs) == 0 {
		return nil
	}
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[strings.ToLower(x)] = true
	}
	return m
}

// mergeAllowedDomains combines domains from config file and ALLOWED_DOMAINS env var
func mergeAllowedDomains(configDomains []string, envDomains string) map[string]bool {
	m := make(map[string]bool)
	// Add from config file
	for _, d := range configDomains {
		d = strings.ToLower(strings.TrimSpace(d))
		if d != "" {
			m[d] = true
		}
	}
	// Add from environment variable (comma-separated)
	if envDomains != "" {
		for _, d := range strings.Split(envDomains, ",") {
			d = strings.ToLower(strings.TrimSpace(d))
			if d != "" {
				m[d] = true
			}
		}
	}
	if len(m) == 0 {
		return nil
	}
	return m
}
func toSetExact(xs []string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}
func normalizeMapLower(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[strings.ToLower(k)] = v
	}
	return out
}
func defaultStr(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
func clampPosInt(n, lo, hi int) int {
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}
func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func cloneHeaderLower(h http.Header) http.Header {
	out := make(http.Header, len(h))
	for k, vs := range h {
		kl := strings.ToLower(k)
		for _, v := range vs {
			out.Add(kl, v)
		}
	}
	return out
}

func mustLoadConfig(path string) Config {
	var cfg Config
	data, err := os.ReadFile(path)
	if err != nil {
		// optional; not fatal
		return Config{}
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return Config{}
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		log.Printf("[config] unable to read %s: %v", path, err)
		return Config{}
	}
	// normalize
	for i := range cfg.AllowedDomains {
		cfg.AllowedDomains[i] = strings.ToLower(cfg.AllowedDomains[i])
	}
	return cfg
}
