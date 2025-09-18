package main

import (
    "context"
    "fmt"
    "time"

    "github.com/redis/go-redis/v9"
)

// Keys for distributed pipe state
func keyPipeOut(sessionID string) string { return fmt.Sprintf("srel:pipe:%s:out", sessionID) }
func keyPipeStreamMeta(sessionID, streamID string) string {
    return fmt.Sprintf("srel:pipe:%s:stream:%s:meta", sessionID, streamID)
}
func keyPipeStreamCancel(sessionID, streamID string) string {
    return fmt.Sprintf("srel:pipe:%s:stream:%s:cancel", sessionID, streamID)
}

// xaddOutFrame appends a frame to the session outbox (Redis Stream) and refreshes TTL.
func xaddOutFrame(ctx context.Context, sessionID string, f OutFrame) (string, error) {
    streamKey := keyPipeOut(sessionID)
    args := &redis.XAddArgs{
        Stream: streamKey,
        Values: map[string]any{
            "type":               f.Type,
            "streamId":           f.StreamID,
            "dataB64":            f.DataB64,
            "originStatus":       f.OriginStatus,
            "originContentType":  f.OriginContentType,
            "error":              f.Error,
            "done":               boolTo01(f.Done),
        },
        Approx: true,
        // Keep IDs auto, and cap outbox length to a soft max
        MaxLen: int64(pipeOutboxMaxFrames),
    }
    id, err := redisCli.XAdd(ctx, args).Result()
    if err != nil {
        return "", err
    }
    // sliding TTL
    _ = redisCli.Expire(ctx, streamKey, time.Duration(ttlSeconds)*time.Second).Err()

    // Notify waiting recv handlers that data is available
    notifySession(ctx, sessionID)

    return id, nil
}

func boolTo01(b bool) string { if b { return "1" } ; return "0" }
func strToBool01(s string) bool { return s == "1" || s == "true" }

func toString(v any) string {
    if v == nil { return "" }
    switch t := v.(type) {
    case string:
        return t
    case []byte:
        return string(t)
    default:
        return fmt.Sprintf("%v", v)
    }
}

func toInt(s string) int {
    n, _ := strconvAtoi(s)
    return n
}
