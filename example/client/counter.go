package main

import (
	"fmt"
	"log"
	"net/http"
	"sync/atomic"
	"time"
)

const maxSeconds = 60 // shorter demo for curl

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/sse", sseHandler)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "Counter SSE available at /sse")
	})
	addr := ":5000"
	log.Printf("[example-upstream] SSE counter on %s/sse", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("upstream error: %v", err)
	}
}

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
	for atomic.LoadInt64(&counter) < maxSeconds {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			current := atomic.AddInt64(&counter, 1)
			fmt.Fprintf(w, "data: %d\n\n", current)
			flusher.Flush()
		}
	}
	fmt.Fprint(w, "data: done\n\n")
	flusher.Flush()
}
