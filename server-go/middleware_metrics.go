package main

import (
    "expvar"
    "fmt"
    "log"
    "net/http"
    "runtime/debug"
    "time"
)

// Basic expvar-backed counters
var (
    metricsRequests   = expvar.NewMap("requests_total")        // key: path
    metricsLatencyMs  = expvar.NewMap("latency_ms_total")      // key: path
    metricsResponses  = expvar.NewMap("responses_total")       // key: path:status
    metricsPanics     = expvar.NewInt("panics_total")
    pipeRecvFramesTot = expvar.NewInt("pipe_recv_frames_total")

    // BodyDict metrics for JSON-aware request deduplication
    dictPutChunks          = expvar.NewInt("dict_put_chunks_total")        // new chunks stored
    dictPutBytes           = expvar.NewInt("dict_put_bytes_total")         // bytes stored
    dictHitChunks          = expvar.NewInt("dict_hit_chunks_total")        // chunks fetched from cache
    dictMissingErrors      = expvar.NewInt("dict_missing_errors_total")    // cache miss errors
    dictReconstructedBytes = expvar.NewInt("dict_reconstructed_bytes_total") // final body size

    // Wire bytes metrics (measures actual bytes on wire after Brotli compression)
    pipeSendWireBytes = expvar.NewInt("pipe_send_wire_bytes_total") // actual bytes received for /pipe/send
)

// statusWriter captures status code and bytes for metrics
type statusWriter struct {
    http.ResponseWriter
    code  int
    bytes int
}

func (w *statusWriter) WriteHeader(code int) {
    w.code = code
    w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
    if w.code == 0 {
        w.code = http.StatusOK
    }
    n, err := w.ResponseWriter.Write(b)
    w.bytes += n
    return n, err
}

// withMetrics records basic per-path counters and latencies
func withMetrics(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        path := r.URL.Path
        sw := &statusWriter{ResponseWriter: w}
        start := time.Now()
        next.ServeHTTP(sw, r)
        elapsed := time.Since(start).Milliseconds()
        metricsRequests.Add(path, 1)
        metricsLatencyMs.Add(path, elapsed)
        metricsResponses.Add(fmt.Sprintf("%s:%d", path, sw.code), 1)
    })
}

// withRecover prevents panics from crashing the server and tracks a counter
func withRecover(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        defer func() {
            if rec := recover(); rec != nil {
                metricsPanics.Add(1)
                log.Printf("[panic] %v\n%s", rec, debug.Stack())
                http.Error(w, "internal server error", http.StatusInternalServerError)
            }
        }()
        next.ServeHTTP(w, r)
    })
}

