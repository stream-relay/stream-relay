package main

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// pipeHub is a singleton that manages a single Redis PSUBSCRIBE connection
// and dispatches signals to multiple waiting handlers per session.
type pipeHub struct {
	mu sync.RWMutex
	// Map sessionID to a Set of channels to support concurrent/overlapping listeners
	listeners map[string]map[chan struct{}]struct{}
	started   bool
}

var (
	pipehub   = &pipeHub{listeners: make(map[string]map[chan struct{}]struct{})}
	pipehubMu sync.Mutex
)

// Start launches the background Redis PSUBSCRIBE loop
func (h *pipeHub) Start() {
	pipehubMu.Lock()
	if h.started {
		pipehubMu.Unlock()
		return
	}
	h.started = true
	pipehubMu.Unlock()

	go h.run()
}

func (h *pipeHub) run() {
	backoff := time.Second
	maxBackoff := 30 * time.Second

	for {
		// Persistent connection for PubSub
		// Subscribe to pattern: srel:notify:* (matches all session notification channels)
		pubsub := redisCli.PSubscribe(context.Background(), "srel:notify:*")
		ch := pubsub.Channel()
		log.Printf("[pubsub] started pipeHub subscriber")

		for msg := range ch {
			// Reset backoff on successful message
			backoff = time.Second

			// Channel name format: srel:notify:<sessionID>
			sessionID := strings.TrimPrefix(msg.Channel, "srel:notify:")
			if sessionID == "" {
				continue
			}

			h.mu.RLock()
			// Notify ALL listeners for this session
			if chans, ok := h.listeners[sessionID]; ok {
				for c := range chans {
					// Non-blocking send - if listener is busy/full, it's already awake
					select {
					case c <- struct{}{}:
					default:
					}
				}
			}
			h.mu.RUnlock()
		}

		// Channel closed - Redis disconnected, reconnect with backoff
		pubsub.Close()
		log.Printf("[pubsub] pipeHub disconnected, reconnecting in %v...", backoff)
		time.Sleep(backoff)
		backoff = min(backoff*2, maxBackoff)
	}
}

// Subscribe returns a new channel for this specific request and an unsubscribe closure
func (h *pipeHub) Subscribe(sessionID string) (chan struct{}, func()) {
	// Create a unique channel for this specific request
	c := make(chan struct{}, 1)

	h.mu.Lock()
	if _, ok := h.listeners[sessionID]; !ok {
		h.listeners[sessionID] = make(map[chan struct{}]struct{})
	}
	h.listeners[sessionID][c] = struct{}{}
	h.mu.Unlock()

	unsubscribe := func() {
		h.mu.Lock()
		defer h.mu.Unlock()

		if set, ok := h.listeners[sessionID]; ok {
			delete(set, c)
			// Only clean up the session key if NO listeners remain
			if len(set) == 0 {
				delete(h.listeners, sessionID)
			}
		}
	}

	return c, unsubscribe
}

// notifySession publishes a notification that data is available for a session
func notifySession(ctx context.Context, sessionID string) {
	// Fire-and-forget publish
	channel := "srel:notify:" + sessionID
	redisCli.Publish(ctx, channel, "1")
}

// cancelHub manages a single Redis PSUBSCRIBE for all stream cancellations.
// This replaces per-stream Subscribe calls which would exhaust Redis connections.
type cancelHub struct {
	mu        sync.RWMutex
	listeners map[string]map[chan struct{}]struct{} // key: "sessionID:streamID"
	started   bool
}

var (
	cancelhub   = &cancelHub{listeners: make(map[string]map[chan struct{}]struct{})}
	cancelhubMu sync.Mutex
)

func (h *cancelHub) Start() {
	cancelhubMu.Lock()
	if h.started {
		cancelhubMu.Unlock()
		return
	}
	h.started = true
	cancelhubMu.Unlock()

	go h.run()
}

func (h *cancelHub) run() {
	backoff := time.Second
	maxBackoff := 30 * time.Second

	for {
		// Subscribe to pattern: srel:pipe:*:stream:*:cancel
		pubsub := redisCli.PSubscribe(context.Background(), "srel:pipe:*:stream:*:cancel")
		ch := pubsub.Channel()
		log.Printf("[pubsub] started cancelHub subscriber")

		for msg := range ch {
			// Reset backoff on successful message
			backoff = time.Second

			// Channel format: srel:pipe:<sessionID>:stream:<streamID>:cancel
			// Extract sessionID and streamID
			// parts: [srel, pipe, <sessionID>, stream, <streamID>, cancel] = 6 parts
			parts := strings.Split(msg.Channel, ":")
			if len(parts) < 6 {
				continue
			}
			// parts: [srel, pipe, <sessionID>, stream, <streamID>, cancel]
			sessionID := parts[2]
			streamID := parts[4]
			key := sessionID + ":" + streamID

			h.mu.RLock()
			if chans, ok := h.listeners[key]; ok {
				for c := range chans {
					select {
					case c <- struct{}{}:
					default:
					}
				}
			}
			h.mu.RUnlock()
		}

		// Channel closed - Redis disconnected, reconnect with backoff
		pubsub.Close()
		log.Printf("[pubsub] cancelHub disconnected, reconnecting in %v...", backoff)
		time.Sleep(backoff)
		backoff = min(backoff*2, maxBackoff)
	}
}

// SubscribeCancel returns a channel that fires when the stream is cancelled
func (h *cancelHub) SubscribeCancel(sessionID, streamID string) (chan struct{}, func()) {
	key := sessionID + ":" + streamID
	c := make(chan struct{}, 1)

	h.mu.Lock()
	if _, ok := h.listeners[key]; !ok {
		h.listeners[key] = make(map[chan struct{}]struct{})
	}
	h.listeners[key][c] = struct{}{}
	h.mu.Unlock()

	unsubscribe := func() {
		h.mu.Lock()
		defer h.mu.Unlock()

		if set, ok := h.listeners[key]; ok {
			delete(set, c)
			if len(set) == 0 {
				delete(h.listeners, key)
			}
		}
	}

	return c, unsubscribe
}

// xreadOutFramesNonBlocking reads frames without blocking the Redis connection
func xreadOutFramesNonBlocking(ctx context.Context, sessionID, cursor string, count int64) (frames []OutFrame, newCursor string, err error) {
	streamKey := keyPipeOut(sessionID)
	if cursor == "" {
		cursor = "0-0"
	}

	// Block: -1 ensures go-redis does NOT send the BLOCK command (Non-blocking)
	xr := redisCli.XRead(ctx, &redis.XReadArgs{
		Streams: []string{streamKey, cursor},
		Count:   count,
		Block:   -1,
	})

	res, err := xr.Result()
	if err == redis.Nil {
		return nil, cursor, nil
	}
	if err != nil {
		return nil, cursor, err
	}
	if len(res) == 0 || len(res[0].Messages) == 0 {
		return nil, cursor, nil
	}

	msgs := res[0].Messages
	frames = make([]OutFrame, 0, len(msgs))
	for _, m := range msgs {
		var of OutFrame
		of.Type = toString(m.Values["type"])
		of.StreamID = toString(m.Values["streamId"])
		of.DataB64 = toString(m.Values["dataB64"])
		of.OriginContentType = toString(m.Values["originContentType"])
		of.Error = toString(m.Values["error"])
		of.Done = strToBool01(toString(m.Values["done"]))
		of.OriginStatus = toInt(toString(m.Values["originStatus"]))
		frames = append(frames, of)
		newCursor = m.ID
	}

	// Refresh TTL
	_ = redisCli.Expire(ctx, streamKey, time.Duration(ttlSeconds)*time.Second).Err()
	if newCursor == "" {
		newCursor = cursor
	}
	return frames, newCursor, nil
}
