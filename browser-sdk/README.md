# Browser SDK

Browser client for stream-relay with BodyDict compression support.

## Features

- **Timeout Immunity**: Uses short-lived HTTP polls (25s max) - no connection ever exceeds infrastructure timeouts
- **BodyDict Compression**: JSON-aware deduplication reduces bandwidth by ~99% for long LLM conversations
- **Zero Dependencies**: Pure JavaScript, works in any modern browser

## Installation

```html
<script src="./browser-sdk/pipe-client.js"></script>
```

## Quick Start

```javascript
// Create client
const client = PipeClient.createPipeClient({
  serverBase: 'https://relay.example.com',
  bodyDictEnabled: true,    // Enable compression (default)
  bodyDictMinSize: 1024     // Compress values >1KB (default)
});

// Initialize connection
await client.init();

// Make a streaming request
const stream = await client.start({
  url: 'https://api.anthropic.com/v1/messages',
  method: 'POST',
  headers: {
    'Content-Type': 'application/json',
    'x-api-key': 'YOUR_API_KEY',
    'anthropic-version': '2023-06-01'
  },
  body: {
    model: 'claude-sonnet-4-20250514',
    max_tokens: 1024,
    stream: true,
    messages: [
      { role: 'user', content: 'Hello!' }
    ]
  },
  onDelta(chunk, meta) {
    // chunk is Uint8Array
    const text = new TextDecoder().decode(chunk);
    console.log(text);
  },
  onDone(err, meta) {
    if (err) {
      console.error('Stream error:', err);
    } else {
      console.log('Stream complete');
    }
  }
});

// Acknowledge completion (cleans up server resources)
await stream.ack();

// Or cancel the stream
// await stream.cancel();
```

## API Reference

### `PipeClient.createPipeClient(options)`

Creates a new pipe client instance.

**Options:**

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `serverBase` | string | **required** | Relay server URL |
| `waitMs` | number | `25000` | Long-poll wait time in ms |
| `backoffMs` | number | `200` | Retry backoff in ms |
| `batchMs` | number | `15` | Batch delay for send queue |
| `bodyDictEnabled` | boolean | `true` | Enable BodyDict compression |
| `bodyDictMinSize` | number | `1024` | Min size (bytes) to trigger compression |

### `client.init()`

Initializes the client, opens a session with the relay server.

Returns: `Promise<void>`

### `client.start(options)`

Starts a new streaming request.

**Options:**

| Option | Type | Description |
|--------|------|-------------|
| `url` | string | Target URL (the actual API endpoint) |
| `method` | string | HTTP method (default: `'GET'`) |
| `headers` | object | Request headers |
| `body` | any | Request body (object, string, or Uint8Array) |
| `onDelta` | function | Called with each response chunk: `(chunk: Uint8Array, meta: object) => void` |
| `onDone` | function | Called when stream completes: `(error: Error | null, meta: object) => void` |
| `onError` | function | Called on handler errors: `(error: Error) => void` |

**Returns:** `Promise<{ id: string, ack: () => Promise<void>, cancel: () => Promise<void> }>`

### `client.stop()`

Stops the receive loop. Call when done with the client.

## BodyDict Compression

BodyDict automatically compresses JSON request bodies by:

1. Extracting values larger than `bodyDictMinSize` (default 1KB)
2. Hashing each with SHA256 and replacing with `{"$ref": "sha256:..."}`
3. Sending new chunks to server; referencing cached chunks

**Example compression for LLM conversations:**

```javascript
// Round 1: Full message history sent
// Round 2: Only new message sent, previous cached
// Round 100: ~99% bandwidth savings
```

The server caches chunks in Redis with a 3-hour TTL. If chunks are evicted, the client automatically resends them (422 retry mechanism).

## SSE Parsing Example

For LLM APIs that return Server-Sent Events:

```javascript
let buffer = '';

await client.start({
  // ... options
  onDelta(chunk) {
    buffer += new TextDecoder().decode(chunk);

    // Parse SSE events
    const lines = buffer.split('\n');
    buffer = lines.pop(); // Keep incomplete line

    for (const line of lines) {
      if (line.startsWith('data: ')) {
        const data = line.slice(6);
        if (data === '[DONE]') {
          console.log('Stream finished');
        } else {
          try {
            const json = JSON.parse(data);
            // Process JSON chunk
            console.log(json.choices?.[0]?.delta?.content || '');
          } catch (e) {
            // Not JSON, handle as text
          }
        }
      }
    }
  }
});
```

## Error Handling

```javascript
const stream = await client.start({
  // ...
  onDone(err) {
    if (err) {
      // Stream error (upstream failure, timeout, etc.)
      console.error('Stream failed:', err.message);
    }
  },
  onError(err) {
    // Handler error (your callback threw)
    console.error('Handler error:', err);
  }
});
```

## Testing

Run the browser test:

```bash
./example/pipe-browser-test/run.sh
# Open http://127.0.0.1:4001/ in browser
```
