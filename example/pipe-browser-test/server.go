package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"time"
)

const defaultPort = 4001
const sseMaxSeconds = 30 // SSE stream duration

func main() {
	port := defaultPort
	if p := os.Getenv("PORT"); p != "" {
		if v, err := strconv.Atoi(p); err == nil && v > 0 && v < 65536 {
			port = v
		}
	}

	publicDir := resolvePublicDir()

	mux := http.NewServeMux()
	mux.HandleFunc("/echo", echoHandler)
	mux.HandleFunc("/sse", sseHandler)
	mux.Handle("/", http.FileServer(http.Dir(publicDir)))

	addr := fmt.Sprintf(":%d", port)
	log.Printf("[pipe-browser-test] serving on %s", addr)
	log.Printf("[pipe-browser-test] open http://127.0.0.1:%d/ in your browser", port)

	if err := http.ListenAndServe(addr, loggingMiddleware(mux)); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func echoHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	defer r.Body.Close()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Echo-Size", fmt.Sprintf("%d", len(body)))
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

// sseHandler streams SSE events once per second for sseMaxSeconds
// Used to test long-running connections that shouldn't be affected by timeouts
func sseHandler(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	var counter int64
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	ctx := r.Context()
	for atomic.LoadInt64(&counter) < sseMaxSeconds {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			current := atomic.AddInt64(&counter, 1)
			fmt.Fprintf(w, "data: {\"count\": %d, \"max\": %d}\n\n", current, sseMaxSeconds)
			flusher.Flush()
		}
	}
	fmt.Fprint(w, "data: {\"done\": true}\n\n")
	flusher.Flush()
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

func resolvePublicDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "./public"
	}
	exeDir := filepath.Dir(exe)
	public := filepath.Join(exeDir, "..", "public")
	abs, err := filepath.Abs(public)
	if err != nil {
		return public
	}
	return abs
}
