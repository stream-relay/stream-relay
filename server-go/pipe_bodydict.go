package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

const (
	dictTTL        = 3 * time.Hour
	dictMaxBodyMB  = 20
	dictRefPrefix  = "sha256:"
	maxRefsPerRequest = 1000  // Limit refs to prevent DoS
	maxJSONDepth      = 100   // Limit recursion depth
)

// Redis key for storing dict chunks per session
func keyDictChunk(sessionID, hash string) string {
	return fmt.Sprintf("srel:pipe:%s:dict:%s", sessionID, hash)
}

// MissingRefError is returned when referenced chunks are not found in Redis
type MissingRefError struct {
	Refs []string
}

func (e MissingRefError) Error() string { return "missing refs" }

// buildBodyFromDict reconstructs the request body from JSON-aware dict encoding.
// It stores new chunks, resolves $ref placeholders, and returns the original body.
func buildBodyFromDict(ctx context.Context, sessionID string, bd *BodyDict) ([]byte, error) {
	if bd == nil {
		return nil, nil
	}
	if bd.V != 1 && bd.V != 2 {
		return nil, fmt.Errorf("unsupported bodyDict version: %d", bd.V)
	}

	// Decode and store new chunks in Redis
	// v1: values are base64 strings, v2: values are raw binary (BSON Binary type)
	decodedPut := make(map[string][]byte, len(bd.Put))
	if len(bd.Put) > 0 {
		pipe := redisCli.Pipeline()
		for hash, val := range bd.Put {
			var data []byte
			var err error

			switch bd.V {
			case 1:
				// v1: base64-encoded string
				b64str, ok := val.(string)
				if !ok {
					return nil, fmt.Errorf("v1 put value for %s is not a string", hash)
				}
				data, err = base64.StdEncoding.DecodeString(b64str)
				if err != nil {
					return nil, fmt.Errorf("invalid base64 for %s", hash)
				}
			case 2:
				// v2: BSON Binary type (raw bytes)
				switch v := val.(type) {
				case primitive.Binary:
					data = v.Data
				case []byte:
					data = v
				default:
					return nil, fmt.Errorf("v2 put value for %s is not binary (got %T)", hash, val)
				}
			}

			decodedPut[hash] = data
			key := keyDictChunk(sessionID, hash)
			pipe.Set(ctx, key, data, dictTTL)
			dictPutBytes.Add(int64(len(data)))
		}
		if _, err := pipe.Exec(ctx); err != nil {
			return nil, fmt.Errorf("redis error storing chunks: %w", err)
		}
		dictPutChunks.Add(int64(len(bd.Put)))
	}

	// Parse skeleton JSON
	var skeleton interface{}
	if err := json.Unmarshal([]byte(bd.Skeleton), &skeleton); err != nil {
		return nil, fmt.Errorf("invalid skeleton JSON: %w", err)
	}

	// Build chunk map from Put (already decoded above)
	chunks := make(map[string][]byte, len(decodedPut))
	for hash, data := range decodedPut {
		chunks[hash] = data
	}

	// Iteratively collect and fetch refs (chunks may contain nested refs)
	knownRefs := make(map[string]bool)
	for hash := range chunks {
		knownRefs[hash] = true
	}

	// Start with refs from skeleton AND from put chunks (put chunks may contain nested refs)
	pendingRefs := collectRefsLimited(skeleton, maxRefsPerRequest)
	for _, data := range decodedPut {
		if len(pendingRefs) >= maxRefsPerRequest {
			break // Already at limit
		}
		var parsed interface{}
		if err := json.Unmarshal(data, &parsed); err == nil {
			newRefs := collectRefsLimited(parsed, maxRefsPerRequest-len(pendingRefs))
			pendingRefs = append(pendingRefs, newRefs...)
		}
	}
	const maxIterations = 10 // Safety limit for deeply nested refs

	for iter := 0; iter < maxIterations && len(pendingRefs) > 0; iter++ {
		// Find refs we need to fetch (not in Put, not already fetched)
		var refsToFetch []string
		for _, ref := range pendingRefs {
			if !knownRefs[ref] {
				refsToFetch = append(refsToFetch, ref)
				knownRefs[ref] = true // Mark as pending to avoid duplicate fetches
			}
		}

		if len(refsToFetch) == 0 {
			break // All refs already loaded
		}

		// Fetch from Redis
		pipe := redisCli.Pipeline()
		cmds := make(map[string]*stringCmd, len(refsToFetch))
		for _, ref := range refsToFetch {
			key := keyDictChunk(sessionID, ref)
			cmds[ref] = &stringCmd{pipe.Get(ctx, key)}
			pipe.Expire(ctx, key, dictTTL) // Refresh TTL
		}
		_, _ = pipe.Exec(ctx)

		var missing []string
		var newChunks [][]byte
		for ref, cmd := range cmds {
			data, err := cmd.cmd.Bytes()
			if err != nil {
				missing = append(missing, ref)
				continue
			}
			chunks[ref] = data
			newChunks = append(newChunks, data)
			dictHitChunks.Add(1)
		}

		if len(missing) > 0 {
			dictMissingErrors.Add(1)
			return nil, MissingRefError{Refs: missing}
		}

		// Collect refs from newly fetched chunks (they may reference other chunks)
		pendingRefs = nil
		for _, data := range newChunks {
			if len(pendingRefs) >= maxRefsPerRequest {
				break
			}
			var parsed interface{}
			if err := json.Unmarshal(data, &parsed); err == nil {
				newRefs := collectRefsLimited(parsed, maxRefsPerRequest-len(pendingRefs))
				pendingRefs = append(pendingRefs, newRefs...)
			}
		}
	}

	// Resolve all refs in the skeleton
	resolved, err := resolveRefs(skeleton, chunks)
	if err != nil {
		return nil, err
	}

	// Marshal back to JSON
	body, err := json.Marshal(resolved)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal resolved body: %w", err)
	}

	if len(body) > dictMaxBodyMB*1024*1024 {
		return nil, fmt.Errorf("reconstructed body too large: %d bytes", len(body))
	}

	dictReconstructedBytes.Add(int64(len(body)))
	return body, nil
}

// stringCmd wraps redis.StringCmd for pipeline results
type stringCmd struct {
	cmd interface{ Bytes() ([]byte, error) }
}

// collectRefsLimited walks the JSON tree and collects $ref values up to maxRefs
func collectRefsLimited(v interface{}, maxRefs int) []string {
	var refs []string
	walkJSONLimited(v, 0, func(node interface{}) bool {
		if len(refs) >= maxRefs {
			return false // Stop walking
		}
		if m, ok := node.(map[string]interface{}); ok {
			if ref, ok := m["$ref"].(string); ok && strings.HasPrefix(ref, dictRefPrefix) {
				refs = append(refs, ref)
			}
		}
		return true // Continue walking
	})
	return refs
}

// walkJSONLimited visits all nodes in a JSON value with depth limit
func walkJSONLimited(v interface{}, depth int, visit func(interface{}) bool) {
	if depth > maxJSONDepth {
		return // Depth limit exceeded
	}
	if !visit(v) {
		return // Visitor requested stop
	}
	switch val := v.(type) {
	case map[string]interface{}:
		for _, child := range val {
			walkJSONLimited(child, depth+1, visit)
		}
	case []interface{}:
		for _, child := range val {
			walkJSONLimited(child, depth+1, visit)
		}
	}
}

// resolveRefs recursively replaces {"$ref": "sha256:..."} with the actual JSON value
func resolveRefs(v interface{}, chunks map[string][]byte) (interface{}, error) {
	switch val := v.(type) {
	case map[string]interface{}:
		// Check if this is a ref node
		if ref, ok := val["$ref"].(string); ok && len(val) == 1 && strings.HasPrefix(ref, dictRefPrefix) {
			data, ok := chunks[ref]
			if !ok {
				return nil, MissingRefError{Refs: []string{ref}}
			}
			// Parse the chunk as JSON
			var parsed interface{}
			if err := json.Unmarshal(data, &parsed); err != nil {
				return nil, fmt.Errorf("invalid JSON in chunk %s: %w", ref, err)
			}
			// Recursively resolve refs in the parsed value
			return resolveRefs(parsed, chunks)
		}
		// Regular object - resolve refs in children
		result := make(map[string]interface{}, len(val))
		for k, child := range val {
			resolved, err := resolveRefs(child, chunks)
			if err != nil {
				return nil, err
			}
			result[k] = resolved
		}
		return result, nil

	case []interface{}:
		result := make([]interface{}, len(val))
		for i, child := range val {
			resolved, err := resolveRefs(child, chunks)
			if err != nil {
				return nil, err
			}
			result[i] = resolved
		}
		return result, nil

	default:
		return v, nil
	}
}
