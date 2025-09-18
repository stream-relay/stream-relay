// client/pipe-client.js
// Node 18+ paired long-poll client for the local proxy.

const crypto = require('crypto');
const zlib = require('zlib');
const { BSON, Binary } = require('bson');

// Fix socket starvation: Node's native fetch (undici) has limited connection pooling.
// Without this, recvLoop holds the socket for 30s (long poll), blocking flush().
// We use undici's fetch with an explicit dispatcher to ensure parallel connections.
const { fetch: undiciFetch, Agent } = require('undici');

// Create a dedicated dispatcher with high connection limits.
// This allows recv (long-poll) and send to use separate sockets.
const dispatcher = new Agent({
    connections: 100,        // Allow many concurrent connections
    pipelining: 0,           // Disable HTTP pipelining for compatibility
    keepAliveTimeout: 30000,
    keepAliveMaxTimeout: 30000,
});

function b64(buf) {
  return Buffer.isBuffer(buf) ? buf.toString('base64') : Buffer.from(buf).toString('base64');
}
function sleep(ms) { return new Promise(r => setTimeout(r, ms)); }

// Create AbortController with timeout
function createTimeoutSignal(ms) {
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), ms);
  return { signal: controller.signal, clear: () => clearTimeout(timeout) };
}

// Drain response body to allow connection reuse
async function drain(resp) {
  try { await resp.arrayBuffer(); } catch {}
}

// BodyDict configuration
const useBodyDict = process.env.PIPE_BODYDICT !== '0'; // enabled by default
const dictMinSize = parseInt(process.env.PIPE_DICT_MIN_SIZE || '1024', 10); // 1KB threshold

// Brotli configuration
const useBrotli = process.env.PIPE_BROTLI !== '0'; // enabled by default
const brotliQuality = parseInt(process.env.PIPE_BROTLI_QUALITY || '11', 10); // max quality by default

// BSON configuration - eliminates base64 overhead for BodyDict
const useBson = process.env.PIPE_BSON !== '0'; // enabled by default

// Compute sha256 hash of a string/buffer
function sha256(data) {
  const buf = typeof data === 'string' ? Buffer.from(data, 'utf8') : data;
  return 'sha256:' + crypto.createHash('sha256').update(buf).digest('hex');
}

// Extract large JSON values (>threshold) into a dictionary.
// Returns {skeleton, chunks} where skeleton has $ref placeholders and chunks is {hash: jsonString}
function extractLargeValues(obj, knownRefs, threshold) {
  const chunks = {};

  function process(val) {
    if (val === null || typeof val !== 'object') {
      return val;
    }

    if (Array.isArray(val)) {
      return val.map(item => process(item));
    }

    // Process object: first recurse into children
    const processed = {};
    for (const [k, v] of Object.entries(val)) {
      processed[k] = process(v);
    }

    // Check if this processed object is large enough to extract
    const serialized = JSON.stringify(processed);
    if (serialized.length >= threshold) {
      const hash = sha256(serialized);
      if (!knownRefs.has(hash)) {
        chunks[hash] = serialized;
      }
      return { $ref: hash };
    }

    return processed;
  }

  const skeleton = process(obj);
  return { skeleton, chunks };
}

function createPipeClient({ serverBase, waitMs = 25000, backoffMs = 200, batchMs = 15, maxStatsHistory = 100, profiling = false }) {
  if (!serverBase) throw new Error('serverBase required');
  const base = serverBase.replace(/\/+$/, '');

  const MAX_BUFFERED_BYTES = 16 * 1024 * 1024;
  const MAX_STREAM_BUFFERED_BYTES = 4 * 1024 * 1024; // Per-stream limit to prevent head-of-line blocking
  const MAX_KNOWN_REFS = 10000; // Cap knownRefs to prevent unbounded growth
  const MAX_BATCH_SIZE = 50; // Cap frames per flush to prevent huge requests

  const state = {
    sessionId: null,
    sessionEpoch: 0,       // increments on each session open, used to guard resets
    cursor: '0-0',
    running: false,
    handlers: new Map(),
    sendQueue: [],
    sendTimer: null,
    sending: false,
    initPromise: null,
    totalBufferedBytes: 0,
    knownRefs: new Set(), // refs known to be stored server-side
  };

  // Fail all handlers when session is invalidated - prevents hung requests
  function failAllHandlers(err) {
    for (const [id, h] of state.handlers) {
      try {
        if (h.onDone) h.onDone(err);
      } catch (e) {
        console.error('[pipe] failAllHandlers onDone error:', e);
      }
      state.handlers.delete(id);
    }
    state.totalBufferedBytes = 0;
  }

  // Stats tracking
  const stats = {
    requests: [],           // recent requests (circular buffer)
    totalRequests: 0,
    totalOriginalBytes: 0,  // before BodyDict
    totalWireBytes: 0,      // after BodyDict + Brotli
    totalResponseBytes: 0,
  };

  // Detailed profiling data (if enabled)
  const profilerData = {
    requests: [],           // detailed request profiles (last 100)
    enabled: profiling,
  };

  // Error log for debugging (ring buffer)
  const MAX_ERROR_LOG = 100;
  const errorLog = [];
  function logClientError(source, message, details = '') {
    const entry = { time: new Date().toISOString(), source, message, details };
    errorLog.push(entry);
    if (errorLog.length > MAX_ERROR_LOG) errorLog.shift();
    console.error(`[pipe] [${source}] ${message}${details ? ': ' + details : ''}`);
  }

  function recordRequestStats(entry) {
    stats.requests.push(entry);
    if (stats.requests.length > maxStatsHistory) {
      stats.requests.shift();
    }
    stats.totalRequests++;
    stats.totalOriginalBytes += entry.originalBytes;
    stats.totalWireBytes += entry.wireBytes;
  }

  function recordProfilingData(profile) {
    if (!profilerData.enabled) return;
    profilerData.requests.push(profile);
    if (profilerData.requests.length > 100) {
      profilerData.requests.shift();
    }
  }

  function recordResponseBytes(bytes) {
    stats.totalResponseBytes += bytes;
  }

  async function open() {
    const maxAttempts = 3;
    let lastErr;
    for (let attempt = 1; attempt <= maxAttempts; attempt++) {
      const { signal, clear } = createTimeoutSignal(30000); // 30s timeout
      try {
        const resp = await undiciFetch(`${base}/pipe/open`, {
          method: 'POST',
          dispatcher,
          signal,
        });
        if (!resp.ok) {
          await drain(resp);
          throw new Error(`open failed: ${resp.status}`);
        }
        const { sessionId } = await resp.json();
        state.sessionId = sessionId;
        state.sessionEpoch++;  // new session = new epoch
        state.cursor = '0-0';
        state.knownRefs.clear();
        return; // success
      } catch (err) {
        lastErr = err;
        if (err.name === 'AbortError') {
          console.warn(`[pipe] open() timeout (attempt ${attempt}/${maxAttempts})`);
          if (attempt < maxAttempts) {
            await sleep(attempt * 1000);
            continue;
          }
          throw new Error(`open timed out after ${maxAttempts} attempts`);
        }
        logClientError('open', 'error', err.message);
        throw err; // non-timeout error, don't retry
      } finally {
        clear();
      }
    }
    throw lastErr;
  }

  function scheduleFlush() {
    if (state.sendTimer || state.sending) return;
    state.sendTimer = setTimeout(flush, Math.max(0, batchMs|0));
  }

  // Build frame with bodyDict at send time (allows rebuild on retry)
  function buildFrame(entry) {
    // Only process start frames with body that should use dict
    if (entry.frame.type !== 'start' || !entry._body || !entry._useDict) {
      return { frame: entry.frame, newRefs: [] };
    }

    try {
      const parsed = JSON.parse(entry._body);
      const { skeleton, chunks } = extractLargeValues(parsed, state.knownRefs, dictMinSize);

      const put = {};
      const newRefs = [];
      for (const [hash, jsonStr] of Object.entries(chunks)) {
        if (useBson) {
          // BSON Binary - no base64 overhead
          put[hash] = new Binary(Buffer.from(jsonStr, 'utf8'));
        } else {
          // JSON with base64
          put[hash] = Buffer.from(jsonStr, 'utf8').toString('base64');
        }
        newRefs.push(hash);
      }

      const frame = {
        ...entry.frame,
        payload: {
          ...entry.frame.payload,
          bodyDict: { v: useBson ? 2 : 1, skeleton: JSON.stringify(skeleton), put }
        }
      };

      return { frame, newRefs };
    } catch (e) {
      // Fall back to bodyB64
      const frame = {
        ...entry.frame,
        payload: {
          ...entry.frame.payload,
          bodyB64: b64(entry._body)
        }
      };
      return { frame, newRefs: [] };
    }
  }

  let openFailures = 0;
  const MAX_OPEN_FAILURES = 10; // After 10 consecutive failures, reject pending items

  async function flush() {
    if (state.sending) return;
    const timer = state.sendTimer; state.sendTimer = null; if (timer) clearTimeout(timer);

    if (!state.sessionId) {
      try {
        if (!state.initPromise) state.initPromise = open();
        await state.initPromise;
        openFailures = 0; // Reset on success
      } catch (e) {
        openFailures++;
        if (openFailures >= MAX_OPEN_FAILURES) {
          // Too many failures - reject all pending items
          logClientError('open', `failed ${openFailures} times`, `rejecting ${state.sendQueue.length} pending items`);
          const err = new Error(`session open failed after ${openFailures} attempts: ${e.message}`);
          for (const item of state.sendQueue) {
            item.reject(err);
          }
          state.sendQueue = [];
          openFailures = 0;
        } else {
          state.sendTimer = setTimeout(flush, 1000);
        }
        state.initPromise = null;
        return;
      } finally {
        state.initPromise = null;
      }
    }

    const batch = state.sendQueue.splice(0, Math.min(state.sendQueue.length, MAX_BATCH_SIZE));
    if (batch.length === 0) return;
    state.sending = true;

    let attempt = 0;
    while (true) {
      try {
        // Build frames fresh on each attempt (allows rebuild after 422)
        const built = batch.map(e => buildFrame(e));
        const frames = built.map(b => b.frame);
        const body = { sessionId: state.sessionId, frames };

        // Serialize as BSON or JSON
        let serialized, serializedBytes, contentType;
        if (useBson) {
          serialized = BSON.serialize(body);
          serializedBytes = serialized.length;
          contentType = 'application/bson';
        } else {
          serialized = Buffer.from(JSON.stringify(body), 'utf8');
          serializedBytes = serialized.length;
          contentType = 'application/json';
        }

        // Optionally compress with Brotli
        let wireBody, wireBytes, headers;
        if (useBrotli) {
          const compressed = zlib.brotliCompressSync(serialized, {
            params: { [zlib.constants.BROTLI_PARAM_QUALITY]: brotliQuality }
          });
          wireBody = compressed;
          wireBytes = compressed.length;
          headers = { 'content-type': contentType, 'content-encoding': 'br' };
        } else {
          wireBody = serialized;
          wireBytes = serializedBytes;
          headers = { 'content-type': contentType };
        }

        const { signal, clear } = createTimeoutSignal(60000); // 60s timeout for send
        let resp;
        try {
          resp = await undiciFetch(`${base}/pipe/send`, {
            method: 'POST',
            headers,
            body: wireBody,
            dispatcher,
            signal,
          });
        } finally {
          clear();
        }

        if (resp.status === 400 || resp.status === 404) {
          await drain(resp);
          throw new Error('Session invalid');
        }

        // Handle 422 missing_refs: remove missing refs and retry
        if (resp.status === 422) {
          const errBody = await resp.json().catch(() => ({})); // json() drains body
          if (errBody.error === 'missing_refs' && Array.isArray(errBody.refs)) {
            console.warn('[pipe] 422 missing_refs, will resend chunks:', errBody.refs.length);
            // Remove missing refs so they get included in put on retry
            for (const ref of errBody.refs) {
              state.knownRefs.delete(ref);
            }
            attempt++;
            if (attempt > 3) {
              logClientError('send', 'missing_refs failed', `after ${attempt} attempts`);
              for (const e of batch) e.reject(new Error('missing_refs after retries'));
              break;
            }
            continue; // Retry with rebuilt frames
          }
        }

        if (!resp.ok) {
          await drain(resp);
          throw new Error(`send failed: ${resp.status}`);
        }

        // Drain successful response to allow connection reuse
        await drain(resp);

        // Success - add new refs to knownRefs
        for (let i = 0; i < batch.length; i++) {
          for (const ref of built[i].newRefs) {
            state.knownRefs.add(ref);
          }
          batch[i].resolve(true);
        }

        // Cap knownRefs size - remove oldest entries if exceeded
        if (state.knownRefs.size > MAX_KNOWN_REFS) {
          const excess = state.knownRefs.size - MAX_KNOWN_REFS;
          let count = 0;
          for (const ref of state.knownRefs) {
            if (count++ >= excess) break;
            state.knownRefs.delete(ref);
          }
        }

        // Record stats per batch (not per frame) for accurate compression ratio
        const startFrames = batch.filter(e => e.frame.type === 'start');
        if (startFrames.length > 0) {
          const totalOriginalBytes = startFrames.reduce((sum, e) => {
            return sum + (e._body ? Buffer.byteLength(e._body, 'utf8') : 0);
          }, 0);
          const urls = startFrames.map(e => e.frame.payload?.target || '').join(', ');
          const statsEntry = {
            timestamp: Date.now(),
            url: startFrames.length === 1 ? urls : `[${startFrames.length} requests]`,
            method: startFrames.length === 1 ? (startFrames[0].frame.payload?.method || 'GET') : 'BATCH',
            originalBytes: totalOriginalBytes,
            serializedBytes,
            wireBytes,
            compressionRatio: totalOriginalBytes > 0 ? (wireBytes / totalOriginalBytes * 100).toFixed(1) : 0,
          };
          recordRequestStats(statsEntry);

          // Record detailed profiling data if enabled
          if (profilerData.enabled && startFrames.length === 1) {
            try {
              const frame = startFrames[0];
              const bodyDictPayload = frame.frame.payload?.bodyDict;

              // Helper to get chunk size (handles BSON Binary objects)
              const getChunkSize = (data) => {
                if (typeof data === 'string') return Buffer.byteLength(data, 'base64');
                if (Buffer.isBuffer(data)) return data.length;
                if (data && typeof data === 'object' && data.buffer) return data.buffer.length; // BSON Binary
                return 0;
              };

              // Safe JSON parse helper
              const safeJsonParse = (str) => {
                try { return JSON.parse(str); } catch { return null; }
              };

              recordProfilingData({
                timestamp: new Date(statsEntry.timestamp).toISOString(),
                url: urls,
                method: frame.frame.payload?.method || 'GET',
                originalBody: frame._body ? safeJsonParse(frame._body) : null,
                skeleton: bodyDictPayload?.skeleton ? safeJsonParse(bodyDictPayload.skeleton) : null,
                chunks: Array.from(Object.entries(bodyDictPayload?.put || {})).map(([hash, data]) => ({
                  hash,
                  size: getChunkSize(data),
                  dataType: typeof data === 'string' ? 'base64' : 'binary',
                })),
                sizes: {
                  originalBytes: totalOriginalBytes,
                  skeletonBytes: bodyDictPayload?.skeleton ? Buffer.byteLength(bodyDictPayload.skeleton, 'utf8') : 0,
                  chunksBytes: bodyDictPayload ? Object.values(bodyDictPayload.put).reduce((sum, data) => {
                    return sum + getChunkSize(data);
                  }, 0) : 0,
                  serializedBytes,
                  wireBytes,
                },
                compression: {
                  bsonMode: useBson,
                  brotliEnabled: useBrotli,
                  ratio: totalOriginalBytes > 0 ? (wireBytes / totalOriginalBytes * 100).toFixed(1) : 0,
                }
              });
            } catch (profileErr) {
              // Ignore profiling errors - don't break the main flow
            }
          }
        }
        break;
      } catch (err) {
        attempt++;
        if (attempt > 3) {
          const finalErr = err.name === 'AbortError'
            ? new Error('send timed out after 3 attempts')
            : err;
          console.warn(`[pipe] send() failed after 3 attempts:`, err.message);
          for (const e of batch) e.reject(finalErr);
          break;
        }
        if (err.name === 'AbortError') {
          logClientError('send', `timeout (attempt ${attempt}/3)`, '');
        } else {
          logClientError('send', `error (attempt ${attempt}/3)`, err.message);
        }
        if (err.message === 'Session invalid') {
          // Fail all existing handlers - they won't receive data from old session
          failAllHandlers(new Error('session invalidated'));
          state.sessionId = null;
          state.knownRefs.clear();
          try {
            if (!state.initPromise) state.initPromise = open();
            await state.initPromise;
          } catch (reOpenErr) {
            for (const e of batch) e.reject(reOpenErr);
            break;
          } finally {
            state.initPromise = null;
          }
        } else {
          await sleep(attempt * 200);
        }
      }
    }
    state.sending = false;
    if (state.sendQueue.length) scheduleFlush();
  }

  function send(frames) {
    const promises = [];
    for (const frame of frames) {
      promises.push(new Promise((resolve, reject) => {
        state.sendQueue.push({ frame, resolve, reject });
      }));
    }
    scheduleFlush();
    return Promise.all(promises);
  }

  async function processQueue(h, streamId) {
    if (h.processing) return;
    h.processing = true;
    while (h.queue.length > 0) {
      const item = h.queue.shift();
      try {
        if (item.type === 'data') {
          h.bufferedBytes -= item.chunk.length;
          if (h.bufferedBytes < 0) h.bufferedBytes = 0;
          state.totalBufferedBytes -= item.chunk.length;
          if (state.totalBufferedBytes < 0) state.totalBufferedBytes = 0;
          if (h.onDelta) await h.onDelta(item.chunk, item.meta);
        } else if (item.type === 'done') {
          if (h.onDone) await h.onDone(null, item.meta);
          state.handlers.delete(streamId);
          return;
        } else if (item.type === 'error') {
          if (h.onDone) await h.onDone(item.error, item.meta);
          state.handlers.delete(streamId);
          return;
        }
      } catch (e) {
        if (h.onError) h.onError(e);
      }
    }
    h.processing = false;
  }

  async function recvLoop() {
    state.running = true;
    let consecutiveFailures = 0;
    const maxBackoff = 30000; // Cap at 30s

    while (state.running) {
      try {
        if (state.totalBufferedBytes > MAX_BUFFERED_BYTES) {
          await sleep(100);
          continue;
        }

        if (!state.sessionId) {
          if (!state.initPromise) state.initPromise = open();
          try {
            await state.initPromise;
          } finally {
            state.initPromise = null;
          }
        }

        // Capture epoch before request - used to guard session reset
        const epochBeforeRequest = state.sessionEpoch;
        const sidBeforeRequest = state.sessionId;

        const body = { sessionId: state.sessionId, cursor: state.cursor, waitMs };
        const { signal, clear } = createTimeoutSignal(waitMs + 10000); // waitMs + 10s buffer
        let resp;
        try {
          resp = await undiciFetch(`${base}/pipe/recv`, {
            method: 'POST',
            headers: { 'content-type': 'application/json' },
            body: JSON.stringify(body),
            dispatcher,
            signal,
          });
        } finally {
          clear();
        }

        if (resp.status === 400 || resp.status === 404 || resp.status === 500) {
          await drain(resp);
          // Only reset session if epoch hasn't changed (avoids race with flush reopening)
          if (state.sessionEpoch === epochBeforeRequest && state.sessionId === sidBeforeRequest) {
            failAllHandlers(new Error(`recv fatal: ${resp.status}`));
            state.sessionId = null;
          }
          throw new Error(`recv fatal: ${resp.status}`);
        }
        if (!resp.ok) {
          await drain(resp);
          throw new Error(`recv failed: ${resp.status}`);
        }

        // Success - reset backoff
        consecutiveFailures = 0;

        const { cursor, frames } = await resp.json(); // json() drains body
        state.cursor = cursor || state.cursor;

        if (Array.isArray(frames)) {
          for (const f of frames) {
            const h = state.handlers.get(f.streamId);
            if (!h) continue;

            if (f.type === 'delta') {
              if (f.dataB64 && f.dataB64.length) {
                const chunk = Buffer.from(f.dataB64, 'base64');
                h.bufferedBytes += chunk.length;
                state.totalBufferedBytes += chunk.length;
                recordResponseBytes(chunk.length);

                // Per-stream limit: if one slow consumer exceeds limit, cancel it
                // This prevents head-of-line blocking where one slow stream stalls all others
                if (h.bufferedBytes > MAX_STREAM_BUFFERED_BYTES) {
                  console.warn(`[pipe] stream ${f.streamId} exceeded buffer limit, cancelling`);
                  try { if (h.onDone) h.onDone(new Error('stream buffer overflow')); } catch {}
                  state.totalBufferedBytes -= h.bufferedBytes;
                  state.handlers.delete(f.streamId);
                  // Queue cancel frame and flush immediately
                  state.sendQueue.push({ frame: { type: 'cancel', streamId: f.streamId }, resolve: () => {}, reject: () => {} });
                  scheduleFlush();
                  continue;
                }

                h.queue.push({ type: 'data', chunk, meta: f });
              }
              if (f.done) {
                h.queue.push({ type: 'done', meta: f });
              }
              processQueue(h, f.streamId);
            } else if (f.type === 'error') {
              h.queue.push({ type: 'error', error: new Error(f.error || 'upstream error'), meta: f });
              processQueue(h, f.streamId);
            }
          }
        }
      } catch (err) {
        consecutiveFailures++;
        // Exponential backoff: 200ms, 400ms, 800ms, ... capped at 30s
        const currentBackoff = Math.min(backoffMs * Math.pow(2, consecutiveFailures - 1), maxBackoff);

        if (err.name === 'AbortError') {
          // Normal timeout - just retry silently
        } else {
          // Only log on first failure or every 10th failure to reduce spam
          if (consecutiveFailures === 1 || consecutiveFailures % 10 === 0) {
            logClientError('recv', `error (attempt ${consecutiveFailures})`, `${err.message}, next retry in ${(currentBackoff/1000).toFixed(1)}s`);
          }
        }
        await sleep(currentBackoff);
      }
    }
  }

  function randomId() {
    return crypto.randomUUID ? crypto.randomUUID() : crypto.randomBytes(16).toString('hex');
  }

  return {
    async init() {
      if (!state.sessionId) {
        if (!state.initPromise) state.initPromise = open();
        try {
          await state.initPromise;
        } finally {
          state.initPromise = null;
        }
      }
      if (!state.running) recvLoop();
    },

    async start({ url, method = 'GET', headers = {}, body = null, onDelta, onDone, onError }) {
      await this.init();
      const streamId = randomId();
      state.handlers.set(streamId, { onDelta, onDone, onError, queue: [], processing: false, bufferedBytes: 0 });

      const frame = {
        type: 'start',
        streamId,
        payload: { target: url, method, headers }
      };

      // Determine if we should use bodyDict (defer actual building to flush)
      let useDict = false;
      let bodyStr = null;

      if (body) {
        // Normalize body to string
        if (typeof body === 'string') {
          bodyStr = body;
        } else if (Buffer.isBuffer(body)) {
          bodyStr = body.toString('utf8');
        } else {
          bodyStr = JSON.stringify(body);
        }
        if (useBodyDict && bodyStr.length >= dictMinSize) {
          try {
            JSON.parse(bodyStr); // Validate JSON
            useDict = true;
          } catch (e) {
            // Not JSON, use bodyB64
            frame.payload.bodyB64 = b64(Buffer.from(bodyStr, 'utf8'));
          }
        } else {
          frame.payload.bodyB64 = b64(Buffer.from(bodyStr, 'utf8'));
        }
        // Keep bodyStr for stats tracking (don't null it out)
      }

      try {
        await new Promise((resolve, reject) => {
          state.sendQueue.push({
            frame,
            resolve,
            reject,
            _body: bodyStr,
            _useDict: useDict
          });
          scheduleFlush();
        });
      } catch (err) {
        // Clean up handler if send failed
        state.handlers.delete(streamId);
        throw err;
      }

      return {
        id: streamId,
        ack: async () => { await send([{ type: 'ack', streamId }]); },
        cancel: async () => { await send([{ type: 'cancel', streamId }]); },
      };
    },

    stop() { state.running = false; },

    getStats() {
      return {
        totalRequests: stats.totalRequests,
        totalOriginalBytes: stats.totalOriginalBytes,
        totalWireBytes: stats.totalWireBytes,
        totalResponseBytes: stats.totalResponseBytes,
        compressionRatio: stats.totalOriginalBytes > 0
          ? ((stats.totalWireBytes / stats.totalOriginalBytes) * 100).toFixed(1)
          : '0.0',
        recentRequests: stats.requests.slice(-20), // last 20
      };
    },

    getErrors(n = 50) {
      const count = Math.min(n, errorLog.length);
      // Return most recent errors first
      return errorLog.slice(-count).reverse();
    },

    getProfilingData() {
      return profilerData;
    }
  };
}

module.exports = { createPipeClient };
