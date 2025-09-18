package main

import (
    "bufio"
    "bytes"
    "context"
    "errors"
    "fmt"
    "io"
    "net/http"
    "time"
)

// Default idle timeout for upstream reads (no data for this long = timeout)
// Set to 5 minutes to accommodate LLM "thinking" time before first token
const upstreamIdleTimeout = 5 * time.Minute

type UpCallbacks struct {
    OnHeaders func(status int, contentType string)
    OnChunk   func(chunk []byte) error
    OnDone    func(finalErr error)
}

// startUpstreamJobBidi mirrors startUpstreamJob but additionally calls callbacks
// to feed a session outbox immediately while also persisting to Redis.
func startUpstreamJobBidi(ctx context.Context, id, targetURL, method string, inHeaders http.Header, body []byte, cb UpCallbacks) error {
    chunksKey := fmt.Sprintf("srel:%s:chunks", id)
    metaKey := fmt.Sprintf("srel:%s:meta", id)
    expireAll(ctx, []string{chunksKey, metaKey})

    // Prepare headers (force identity; strip hop-by-hop)
    forwarded := cloneHeaderLower(inHeaders)
    for _, k := range []string{
        "host", "connection", "content-length", "transfer-encoding", "proxy-connection", "upgrade", "accept-encoding", "content-encoding",
    } {
        forwarded.Del(k)
    }
    forwarded.Set("accept-encoding", "identity")
    forwarded.Set("content-encoding", "identity")

    req, err := http.NewRequestWithContext(ctx, method, targetURL, bytes.NewReader(body))
    if err != nil {
        return err
    }
    req.Header = forwarded

    resp, err := httpClient.Do(req)
    if err != nil {
        _ = hsetMeta(ctx, metaKey, map[string]string{
            "status":      "502",
            "contentType": "text/plain; charset=utf-8",
            "done":        "1",
            "error":       err.Error(),
        })
        expireAll(ctx, []string{chunksKey, metaKey})
        if cb.OnDone != nil {
            cb.OnDone(err)
        }
        return err
    }
    defer resp.Body.Close()

    originStatus := resp.StatusCode
    originCT := resp.Header.Get("Content-Type")
    if originCT == "" {
        originCT = "application/octet-stream"
    }

    _ = hsetMeta(ctx, metaKey, map[string]string{
        "status":      fmt.Sprintf("%d", originStatus),
        "contentType": originCT,
        "done":        "0",
    })
    expireAll(ctx, []string{chunksKey, metaKey})

    if cb.OnHeaders != nil {
        cb.OnHeaders(originStatus, originCT)
    }

    reader := bufio.NewReader(resp.Body)
    buf := make([]byte, upstreamChunkSize)
    lastExpire := time.Now()

    // Channel for read results to implement idle timeout
    type readResult struct {
        n   int
        err error
    }

    // Single timer that gets reset - avoids timer leak from time.After() per iteration
    idleTimer := time.NewTimer(upstreamIdleTimeout)
    defer idleTimer.Stop()

    // Reusable channel for read results
    resultCh := make(chan readResult, 1)

    for {
        // Spawn read goroutine
        go func() {
            n, err := reader.Read(buf)
            resultCh <- readResult{n, err}
        }()

        var n int
        var readErr error
        select {
        case <-ctx.Done():
            // Context cancelled (e.g., client sent cancel frame)
            // Must emit terminal frame so clients can clean up handlers
            ctxErr := ctx.Err()
            // Use fresh context for Redis since original is cancelled
            rctx, cancel := redisCtx()
            _ = hsetMeta(rctx, metaKey, map[string]string{"done": "1", "error": "cancelled"})
            expireAll(rctx, []string{chunksKey, metaKey})
            cancel()
            if cb.OnDone != nil {
                cb.OnDone(ctxErr)
            }
            return ctxErr
        case <-idleTimer.C:
            readErr = fmt.Errorf("upstream idle timeout after %v", upstreamIdleTimeout)
            _ = hsetMeta(ctx, metaKey, map[string]string{"done": "1", "error": readErr.Error()})
            expireAll(ctx, []string{chunksKey, metaKey})
            if cb.OnDone != nil {
                cb.OnDone(readErr)
            }
            return readErr
        case result := <-resultCh:
            n = result.n
            readErr = result.err
            // Reset timer for next read - drain channel first if timer already fired
            if !idleTimer.Stop() {
                select {
                case <-idleTimer.C:
                default:
                }
            }
            idleTimer.Reset(upstreamIdleTimeout)
        }

        if n > 0 {
            ch := make([]byte, n)
            copy(ch, buf[:n])
            // removed rpushBuffer to avoid double storage (list + stream)
            if time.Since(lastExpire) > 5*time.Second {
                expireAll(ctx, []string{chunksKey, metaKey})
                lastExpire = time.Now()
            }
            if cb.OnChunk != nil {
                if err := cb.OnChunk(ch); err != nil {
                    // Critical: Redis write failure on pipe, terminate stream
                    _ = hsetMeta(ctx, metaKey, map[string]string{"done": "1", "error": "pipe write failed: " + err.Error()})
                    expireAll(ctx, []string{chunksKey, metaKey})
                    if cb.OnDone != nil {
                        cb.OnDone(err)
                    }
                    return err
                }
            }
        }
        if readErr != nil {
            if errors.Is(readErr, io.EOF) {
                break
            }
            _ = hsetMeta(ctx, metaKey, map[string]string{"done": "1", "error": readErr.Error()})
            expireAll(ctx, []string{chunksKey, metaKey})
            if cb.OnDone != nil {
                cb.OnDone(readErr)
            }
            return readErr
        }
    }

    _ = hsetMeta(ctx, metaKey, map[string]string{"done": "1"})
    expireAll(ctx, []string{chunksKey, metaKey})
    if cb.OnDone != nil {
        cb.OnDone(nil)
    }
    return nil
}
