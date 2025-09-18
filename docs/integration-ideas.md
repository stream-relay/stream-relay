# Integration Ideas: Axios & Fetch Wrappers

The browser SDK (`PipeClient.createPipeClient`) handles streaming through the relay with BodyDict compression. This doc sketches options to make adoption even easier by wrapping common HTTP clients.

## Goals
- Provide minimal-change integration paths for apps already using axios or fetch.
- Preserve streaming semantics when possible.
- Be explicit about limitations (e.g., axios buffering).

## Option A: axios adapter / interceptor

**Idea:** Ship an axios adapter that delegates requests to the pipe client for non-streaming responses.

### Shape
```js
import axios from 'axios';
import { createRelayAxios } from '@stream-relay/axios-adapter';

const relayAxios = createRelayAxios({
  serverBase: 'https://relay.example.com',
  bodyDictEnabled: true,
  bodyDictMinSize: 1024,
});

// Use like normal axios
const res = await relayAxios.post('/foo', { hello: 'world' });
```

### Behavior
- For `responseType` in `['json','text','arraybuffer']`, the adapter calls into the SDK and returns an axios-like response.
- For streaming types, axios (browser) doesn't expose `ReadableStream` or streaming events; the adapter should **throw or warn** and direct users to the SDK for streaming endpoints.

### Pros
- Drop-in for many existing axios call sites.
- Minimal app changes for non-streaming endpoints.
- Benefits from BodyDict compression automatically.

### Cons / Limits
- Browser axios buffers responses; no true streaming via axios. Must steer users to the SDK for streaming.
- Mix of two call styles (axios for non-stream, SDK for stream) may need docs.

## Option B: relayFetch helper (opt-in)

**Idea:** Provide a `relayFetch` function that mirrors the fetch API but routes through the SDK.

### Shape
```js
import { createRelayFetch } from '@stream-relay/fetch';

const relayFetch = createRelayFetch({
  serverBase: 'https://relay.example.com',
  bodyDictEnabled: true,
});

// Use like fetch
const res = await relayFetch('https://api.example.com/foo', {
  method: 'POST',
  body: JSON.stringify(data)
});
```

### Behavior
- For non-streaming, returns a Response-like object (status, headers, json/text/arrayBuffer helpers).
- For streaming, could expose callbacks or a ReadableStream wrapper.

### Pros
- Minimal surface change for apps that can swap `fetch` → `relayFetch`.
- Cleanly supports streaming if we expose the SDK's streaming primitives.
- Automatic BodyDict compression for large request bodies.

### Cons
- If we patch `window.fetch` globally, it can surprise dependencies; better to expose an opt-in function.

## Option C: fetch monkey-patch (not recommended)

**Idea:** `initRelayFetch({ serverBase })` replaces `window.fetch` globally.

### Pros
- Zero call-site changes.

### Cons
- Risky: can break dependencies expecting native fetch semantics.
- Creates hidden coupling.
- Should be avoided or made very explicit if offered.

## Recommended path

1. **Provide `createRelayAxios`** for non-streaming responses with a clear error for streaming, directing users to the SDK.
2. **Provide `createRelayFetch`** as an opt-in fetch-compatible helper with streaming support via SDK primitives.
3. **Avoid monkey-patching** `window.fetch` by default.

## Notes on BodyDict compression

Both wrappers should automatically benefit from BodyDict compression:
- Large JSON request bodies (>1KB values) are deduplicated
- Previous conversation history is referenced by hash
- ~99% bandwidth savings for long LLM conversations

The relay handles all compression/decompression transparently.
