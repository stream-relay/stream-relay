package main

import (
    "context"
    "encoding/base64"
    "encoding/json"
    "errors"
    "fmt"
    "net/http"
    "net/url"
    "os"
    "strconv"
    "strings"
    "time"

    "github.com/andybalholm/brotli"
    "github.com/google/uuid"
    "go.mongodb.org/mongo-driver/bson"
)

// Semaphore to limit concurrent upstream jobs (prevents resource exhaustion)
var maxUpstreamConcurrency = getEnvInt("MAX_UPSTREAM_CONCURRENCY", 100)
var upstreamSem = make(chan struct{}, maxUpstreamConcurrency)

// Timeout for Redis operations in hot paths (prevents pool exhaustion)
const redisOpTimeout = 5 * time.Second

func getEnvInt(key string, defaultVal int) int {
    if v := os.Getenv(key); v != "" {
        if n, err := strconv.Atoi(v); err == nil && n > 0 {
            return n
        }
    }
    return defaultVal
}

// redisCtx creates a context with timeout for Redis operations
func redisCtx() (context.Context, context.CancelFunc) {
    return context.WithTimeout(context.Background(), redisOpTimeout)
}

// ----- /pipe/open -----
func handlePipeOpen(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost {
        http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
        return
    }
    id := uuid.NewString()
    // We do not pre-create Redis keys for sessions; they appear on first frame
    writeJSON(w, http.StatusOK, map[string]any{"sessionId": id, "ttl": ttlSeconds})
}

// ----- /pipe/recv -----
func handlePipeRecv(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost {
        http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
        return
    }
    var req struct {
        SessionID string `json:"sessionId"`
        Cursor    string `json:"cursor"` // Redis Stream ID (e.g. "0-0")
        WaitMs    int64  `json:"waitMs"`
    }
    if err := decodeJSONMaybeBrotli(r, &req); err != nil || req.SessionID == "" {
        http.Error(w, "invalid json", http.StatusBadRequest)
        return
    }

    desiredWait := req.WaitMs
    if desiredWait <= 0 {
        desiredWait = int64(longPollMaxMs)
    }
    waitMs := min64(int64(longPollMaxMs), desiredWait)

    // 1. Subscribe FIRST to catch any notifications that occur during processing
    notifyCh, unsubscribe := pipehub.Subscribe(req.SessionID)
    defer unsubscribe()

    cursor := defaultStr(req.Cursor, "0-0")
    const maxFrames = 256

    // 2. Check for data immediately (Non-blocking)
    // This covers data that arrived before we subscribed or while we were setting up
    frames, newCur, err := xreadOutFramesNonBlocking(r.Context(), req.SessionID, cursor, int64(maxFrames))
    if err != nil {
        http.Error(w, "redis error", http.StatusInternalServerError)
        return
    }

    // 3. If no data, wait for notification
    if len(frames) == 0 {
        timeout := time.NewTimer(time.Duration(waitMs) * time.Millisecond)
        defer timeout.Stop()

        select {
        case <-notifyCh:
            // Signal received! Data is ready.
        case <-timeout.C:
            // Timeout reached, return empty response (client will poll again)
        case <-r.Context().Done():
            // Client disconnected
            return
        }

        // 4. Read again (Non-blocking)
        frames, newCur, err = xreadOutFramesNonBlocking(r.Context(), req.SessionID, cursor, int64(maxFrames))
        if err != nil {
            http.Error(w, "redis error", http.StatusInternalServerError)
            return
        }
    }

    if n := len(frames); n > 0 {
        pipeRecvFramesTot.Add(int64(n))
    }

    // Brotli-compress JSON response (level 11 for maximum compression like requests)
    w.Header().Set("Content-Type", "application/json; charset=utf-8")
    w.Header().Set("Cache-Control", "no-store")
    w.Header().Set("Content-Encoding", "br")
    br := brotli.NewWriterLevel(w, 11)
    defer br.Close()
    resp := map[string]any{"cursor": newCur, "frames": frames}
    _ = json.NewEncoder(br).Encode(resp)
}

// ----- /pipe/send -----
func handlePipeSend(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost {
        http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
        return
    }
    var req struct {
        SessionID string    `json:"sessionId" bson:"sessionId"`
        Frames    []InFrame `json:"frames" bson:"frames"`
    }
    // Read and decompress, tracking wire bytes for compression metrics
    body, wireBytes, err := readMaybeBrotliWithSize(r)
    if err != nil || len(body) == 0 {
        http.Error(w, "invalid request body", http.StatusBadRequest)
        return
    }
    pipeSendWireBytes.Add(wireBytes)

    // Parse as BSON or JSON based on Content-Type
    contentType := r.Header.Get("Content-Type")
    if strings.HasPrefix(contentType, "application/bson") {
        if err := bson.Unmarshal(body, &req); err != nil || req.SessionID == "" {
            http.Error(w, "invalid bson", http.StatusBadRequest)
            return
        }
    } else {
        if err := json.Unmarshal(body, &req); err != nil || req.SessionID == "" {
            http.Error(w, "invalid json", http.StatusBadRequest)
            return
        }
    }

    for _, f := range req.Frames {
        switch f.Type {
        case "start":
            if f.Payload == nil || f.StreamID == "" || f.Payload.Target == "" {
                http.Error(w, "invalid start frame", http.StatusBadRequest)
                return
            }
            // Apply domain translation & allowlist checks
            u, err := url.Parse(f.Payload.Target)
            if err != nil {
                http.Error(w, "bad target", http.StatusBadRequest)
                return
            }
            applyDomainTranslation(u)
            host := strings.ToLower(u.Hostname())
            // Security: require allowlist to be configured (default deny)
            if len(allowSet) == 0 {
                logError("allowlist", "ALLOWED_DOMAINS not configured", fmt.Sprintf("target=%s", f.Payload.Target))
                http.Error(w, "ALLOWED_DOMAINS not configured", http.StatusForbidden)
                return
            }
            if !allowSet[host] {
                logWarn("allowlist", "target domain not allowed", fmt.Sprintf("host=%s target=%s", host, f.Payload.Target))
                http.Error(w, "target domain not allowed", http.StatusForbidden)
                return
            }

            // Build headers from payload
            h := make(http.Header)
            for k, v := range f.Payload.Headers {
                switch val := v.(type) {
                case string:
                    h.Set(k, val)
                case []interface{}:
                    for _, s := range val {
                        if sStr, ok := s.(string); ok {
                            h.Add(k, sStr)
                        }
                    }
                }
            }

            // Decode body: prefer bodyDict (JSON-aware dedup), fallback to bodyB64
            var body []byte
            switch {
            case f.Payload.BodyDict != nil:
                b, err := buildBodyFromDict(r.Context(), req.SessionID, f.Payload.BodyDict)
                if err != nil {
                    var mErr MissingRefError
                    if errors.As(err, &mErr) {
                        writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
                            "error":    "missing_refs",
                            "streamId": f.StreamID,
                            "refs":     mErr.Refs,
                        })
                        return
                    }
                    http.Error(w, "invalid bodyDict: "+err.Error(), http.StatusBadRequest)
                    return
                }
                body = b

            case f.Payload.BodyB64 != "":
                b, err := base64.StdEncoding.DecodeString(f.Payload.BodyB64)
                if err != nil {
                    http.Error(w, "bad bodyB64", http.StatusBadRequest)
                    return
                }
                body = b

            default:
                body = nil
            }

            // Create cancellable context per upstream and spawn job
            jobCtx, cancel := context.WithCancel(context.Background())
            upID := uuid.NewString()

            // Persist minimal per-stream meta for ACK
            metaKey := keyPipeStreamMeta(req.SessionID, f.StreamID)
            _ = hsetMeta(r.Context(), metaKey, map[string]string{"upstreamId": upID})
            expireAll(r.Context(), []string{metaKey})

            // Prepare immutable copies
            urlStr := u.String()
            methodUpper := strings.ToUpper(f.Payload.Method)
            hdr := h
            bodyCopy := body
            sessionID := req.SessionID
            streamID := f.StreamID

            // Watch for cancel flags via shared cancelHub (avoids per-stream Redis connections)
            cancelKey := keyPipeStreamCancel(sessionID, streamID)
            cancelCh, unsubCancel := cancelhub.SubscribeCancel(sessionID, streamID)
            go func(ctx context.Context, key string, cancel func(), cancelCh chan struct{}, unsubCancel func()) {
                defer unsubCancel()

                // 1. Check if already cancelled (race condition safety)
                if redisCli.Exists(context.Background(), key).Val() > 0 {
                    cancel()
                    return
                }

                // 2. Wait for notification from shared cancelHub with periodic poll fallback
                //    (handles pubsub disconnect scenarios)
                const pollInterval = 2 * time.Second
                pollTimer := time.NewTimer(pollInterval)
                defer pollTimer.Stop()

                for {
                    select {
                    case <-ctx.Done():
                        return
                    case <-cancelCh:
                        cancel()
                        return
                    case <-pollTimer.C:
                        // Fallback poll in case pubsub notification is missed
                        if redisCli.Exists(context.Background(), key).Val() > 0 {
                            cancel()
                            return
                        }
                        pollTimer.Reset(pollInterval)
                    }
                }
            }(jobCtx, cancelKey, cancel, cancelCh, unsubCancel)

            // Acquire semaphore slot (limits concurrent upstream jobs)
            // Use timeout to prevent blocking the handler indefinitely
            select {
            case upstreamSem <- struct{}{}:
                // got slot immediately
            case <-time.After(10 * time.Second):
                // Timeout waiting for slot - return 503 to client
                cancel() // Clean up the context
                logError("upstream", "capacity exceeded", fmt.Sprintf("sessionId=%s streamId=%s", req.SessionID, f.StreamID))
                http.Error(w, "upstream capacity exceeded", http.StatusServiceUnavailable)
                return
            }

            go func() {
                defer func() { <-upstreamSem }() // release semaphore slot
                defer cancel() // Fix leak: ensure context is cancelled when job finishes
                var originStatus int
                var originCT string
                cb := UpCallbacks{
                    OnHeaders: func(status int, ct string) {
                        originStatus = status
                        originCT = ct
                        // mirror to meta (with timeout to prevent pool exhaustion)
                        ctx, cancel := redisCtx()
                        defer cancel()
                        _ = hsetMeta(ctx, metaKey, map[string]string{
                            "status": fmt.Sprintf("%d", status),
                            "contentType": ct,
                        })
                        expireAll(ctx, []string{metaKey})
                    },
                    OnChunk: func(chunk []byte) error {
                        ctx, cancel := redisCtx()
                        defer cancel()
                        _, err := xaddOutFrame(ctx, sessionID, OutFrame{
                            Type:              "delta",
                            StreamID:          streamID,
                            DataB64:           base64.StdEncoding.EncodeToString(chunk),
                            OriginStatus:      originStatus,
                            OriginContentType: originCT,
                            Done:              false,
                        })
                        return err
                    },
                    OnDone: func(finalErr error) {
                        ctx, cancel := redisCtx()
                        defer cancel()
                        if finalErr != nil {
                            logError("upstream", "request failed", fmt.Sprintf("streamId=%s url=%s error=%v", streamID, urlStr, finalErr))
                            _, _ = xaddOutFrame(ctx, sessionID, OutFrame{
                                Type:     "error",
                                StreamID: streamID,
                                Error:    finalErr.Error(),
                            })
                        } else {
                            _, _ = xaddOutFrame(ctx, sessionID, OutFrame{
                                Type:              "delta",
                                StreamID:          streamID,
                                DataB64:           "",
                                OriginStatus:      originStatus,
                                OriginContentType: originCT,
                                Done:              true,
                            })
                        }
                    },
                }
                _ = startUpstreamJobBidi(jobCtx, upID, urlStr, methodUpper, hdr, bodyCopy, cb)
            }()

        case "ack":
            // Remove Redis chunk/meta for the upstream job and per-stream meta/cancel
            metaKey := keyPipeStreamMeta(req.SessionID, f.StreamID)
            meta, _ := rdbHGetAll(r.Context(), metaKey)
            upID := meta["upstreamId"]
            if upID != "" {
                keys := []string{fmt.Sprintf("srel:%s:chunks", upID), fmt.Sprintf("srel:%s:meta", upID)}
                go redisCli.Del(context.Background(), keys...)
            }
            // Clean per-stream keys
            redisCli.Del(r.Context(), metaKey)
            redisCli.Del(r.Context(), keyPipeStreamCancel(req.SessionID, f.StreamID))

        case "cancel":
            // Set a cancel flag; background job watcher will observe and cancel
            cancelKey := keyPipeStreamCancel(req.SessionID, f.StreamID)
            pipe := redisCli.Pipeline()
            pipe.Set(r.Context(), cancelKey, "1", time.Duration(ttlSeconds)*time.Second)
            pipe.Publish(r.Context(), cancelKey, "1")
            _, _ = pipe.Exec(r.Context())

        default:
            http.Error(w, "unknown frame type", http.StatusBadRequest)
            return
        }
    }

    writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
