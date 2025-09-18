package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
)

const defaultPort = 3333

func main() {
	port := defaultPort
	if p := os.Getenv("PORT"); p != "" {
		if v, err := strconv.Atoi(p); err == nil && v > 0 && v < 65536 {
			port = v
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/echo", echoHandler)

	addr := fmt.Sprintf(":%d", port)
	log.Printf("[echo] listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, loggingMiddleware(mux)))
}

func echoHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Read request body
	body, err := io.ReadAll(r.Body)
	defer r.Body.Close()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Echo back
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Echo-Size", fmt.Sprintf("%d", len(body)))
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s", r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
	})
}
