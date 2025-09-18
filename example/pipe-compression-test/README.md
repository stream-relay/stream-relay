# Pipe BodyDict Compression Test

This test validates the BodyDict compression feature for the pipe protocol.

## What it tests

- **Small payloads**: No compression (below 1KB threshold)
- **Medium payloads**: Compression with some reusable chunks
- **Large payloads**: Heavy compression with repetitive conversation context
- **Very large payloads**: Maximum compression ratio

The test simulates LLM conversation patterns where large amounts of context are repeated across requests.

## Running the test

```bash
./run.sh
```

This will:
1. Start Redis (if not already running)
2. Build the echo server and relay server
3. Run 4 test scenarios with increasing payload sizes
4. Display compression metrics from the server

## What to look for

- **Chunks stored**: Number of unique chunks extracted and cached
- **Compression ratio**: How much the body was compressed (e.g., `1:5` = 5x compression)
- **Bytes saved**: Total bandwidth saved by reusing chunks from previous requests

## Files

- `run.sh`: Test orchestration script
- `echo-server.go`: Simple HTTP server that echoes POST requests
- `test-compression.js`: Node.js test client using the pipe protocol
- `go.mod`: Go module definition
