// client/client.js
// Local transparent proxy using paired long-poll pipe.
// Node >= 18

const http = require('http');
const { URL } = require('url');
const { createPipeClient } = require('./pipe-client');

const LISTEN_PORT = parseInt(process.env.LISTEN_PORT || '9999', 10);
const STATS_PORT = parseInt(process.env.STATS_PORT || '9998', 10);
const SERVER_BASE = process.env.SERVER_BASE;
const UPSTREAM_BASE = process.env.UPSTREAM_BASE;
const PIPE_PROFILE = process.env.PIPE_PROFILE === '1';
const STREAM_RELAY_QUIET = process.env.STREAM_RELAY_QUIET === '1';

if (!SERVER_BASE) {
    throw new Error('SERVER_BASE is required (e.g., https://relay.example.com)');
}
if (!UPSTREAM_BASE) {
    throw new Error('UPSTREAM_BASE is required (e.g., https://api.example.com)');
}

if (PIPE_PROFILE && !STREAM_RELAY_QUIET) {
    console.log('[client] profiling enabled - detailed request data available at http://127.0.0.1:' + STATS_PORT + '/profile/export');
}

function toPositiveInt(value, fallback) {
    const parsed = parseInt(value, 10);
    if (Number.isNaN(parsed) || parsed <= 0) {
        return fallback;
    }
    return parsed;
}

const LONG_POLL_TIMEOUT_MS = toPositiveInt(process.env.LONG_POLL_TIMEOUT_MS, 30000);

// Simple hop-by-hop header filter
function stripHopByHop(headers) {
    const out = {};
    for (const [k, v] of Object.entries(headers)) {
        const key = k.toLowerCase();
        if (['connection', 'proxy-connection', 'keep-alive', 'transfer-encoding', 'upgrade', 'host', 'content-length'].includes(key)) {
            continue;
        }
        // Support strings or arrays (multi-value)
        out[key] = v; 
    }
    return out;
}

function buildUpstreamUrl(req) {
    // Preserve path + query, swap host to UPSTREAM_BASE
    const base = new URL(UPSTREAM_BASE);
    const u = new URL(req.url, base); // preserves req.url path/query against base
    return u.toString();
}

function responseMustNotHaveBody(statusCode) {
    return (statusCode >= 100 && statusCode < 200) || statusCode === 204 || statusCode === 304;
}

function setResponseHeaders(res, statusCode, contentType) {
    res.statusCode = statusCode || 200;
    if (!responseMustNotHaveBody(res.statusCode)) {
        res.setHeader('Content-Type', contentType || 'application/octet-stream');
    }
}

const MAX_INBOUND_BODY_BYTES = toPositiveInt(process.env.MAX_INBOUND_BODY_BYTES, 10 * 1024 * 1024);

const pipe = createPipeClient({ serverBase: SERVER_BASE, waitMs: LONG_POLL_TIMEOUT_MS, profiling: PIPE_PROFILE });

const server = http.createServer(async (req, res) => {
    let streamHandle = null;
    let closed = false;

    // Mark as closed and cancel upstream if running
    function markClosed() {
        closed = true;
        if (streamHandle) streamHandle.cancel().catch(() => {});
    }

    // Attach close handlers early to catch disconnects during upload or start()
    req.on('aborted', markClosed);
    req.on('close', () => { if (!req.complete) markClosed(); });
    res.on('close', () => { if (!res.writableEnded) markClosed(); });
    res.on('error', markClosed);

    try {
        const method = req.method || 'GET';
        const url = buildUpstreamUrl(req);

        // Buffer the incoming request body with proper abort handling
        const body = await new Promise((resolve, reject) => {
            const chunks = [];
            let total = 0;
            let ended = false;

            function fail(err) {
                if (!ended) {
                    ended = true;
                    reject(err);
                }
            }

            req.on('aborted', () => fail(new Error('client aborted upload')));
            req.on('error', fail);

            req.on('data', (c) => {
                total += c.length;
                if (total > MAX_INBOUND_BODY_BYTES) {
                    req.destroy();
                    fail(new Error('request body too large'));
                    return;
                }
                chunks.push(Buffer.from(c));
            });

            req.on('end', () => {
                ended = true;
                resolve(Buffer.concat(chunks));
            });

            req.on('close', () => {
                if (!ended && !req.complete) fail(new Error('client closed early'));
            });
        });

        // If client closed during body buffering, don't proceed
        if (closed) return;

        const headers = stripHopByHop(req.headers);

        let originStatus, originCT;

        streamHandle = await pipe.start({
            url, method, headers, body,
            async onDelta(chunk, meta) {
                // Check if response is still writable
                if (res.destroyed || res.writableEnded) return;

                if (!res.headersSent) {
                    setResponseHeaders(res, meta.originStatus || 200, meta.originContentType);
                    res.setHeader('Cache-Control', 'no-cache, no-transform');
                    res.setHeader('Connection', 'keep-alive');
                    originStatus = meta.originStatus ?? originStatus;
                    originCT = meta.originContentType ?? originCT;
                }
                const ok = res.write(chunk);
                if (!ok && !res.destroyed) {
                    // Race drain against close to avoid hanging forever
                    await Promise.race([
                        new Promise(resolve => res.once('drain', resolve)),
                        new Promise(resolve => res.once('close', resolve)),
                    ]);
                }
            },
            async onDone(err, meta) {
                try { if (err) console.error('[proxy] upstream error:', err.message); } finally {
                    if (!res.headersSent && !res.destroyed) {
                        // Use meta from terminal frame for empty-body responses (e.g., 204, 500 with no body)
                        const finalStatus = meta?.originStatus ?? originStatus ?? 200;
                        const finalCT = meta?.originContentType ?? originCT;
                        setResponseHeaders(res, err ? 502 : finalStatus, finalCT);
                    }
                    if (!res.writableEnded) {
                        res.end();
                    }
                    if (streamHandle) await streamHandle.ack().catch(() => {});
                }
            },
            onError(e) {
                console.error('[proxy] handler error', e);
            }
        });

        // If client closed during pipe.start(), cancel the stream
        if (closed) {
            streamHandle.cancel().catch(() => {});
        }

    } catch (err) {
        console.error('[client error]', err);
        if (!res.headersSent) {
            res.statusCode = 502;
            res.setHeader('Content-Type', 'text/plain; charset=utf-8');
            res.end('stream-relay client: upstream or relay error');
        } else {
            try { res.end(); } catch {}
        }
    }
});

server.listen(LISTEN_PORT, () => {
    if (!STREAM_RELAY_QUIET) {
        console.log(`[client] listening on http://127.0.0.1:${LISTEN_PORT}`);
    }
});

// Stats dashboard server
const statsHtml = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>Stream Relay Stats</title>
  <style>
    * { box-sizing: border-box; margin: 0; padding: 0; }
    body { font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; background: #1a1a2e; color: #eee; padding: 20px; }
    h1 { color: #00d4ff; margin-bottom: 20px; }
    .stats-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(200px, 1fr)); gap: 15px; margin-bottom: 30px; }
    .stat-card { background: #16213e; border-radius: 10px; padding: 20px; }
    .stat-value { font-size: 2em; font-weight: bold; color: #00d4ff; }
    .stat-label { color: #888; font-size: 0.9em; margin-top: 5px; }
    table { width: 100%; border-collapse: collapse; background: #16213e; border-radius: 10px; overflow: hidden; }
    th, td { padding: 12px 15px; text-align: left; border-bottom: 1px solid #2a2a4a; }
    th { background: #0f3460; color: #00d4ff; }
    tr:hover { background: #1f3a6e; }
    .ratio-good { color: #4ade80; }
    .ratio-ok { color: #fbbf24; }
    .ratio-bad { color: #f87171; }
    .refresh { color: #666; font-size: 0.8em; margin-top: 20px; }
  </style>
</head>
<body>
  <h1>Stream Relay Stats</h1>
  <div class="stats-grid">
    <div class="stat-card">
      <div class="stat-value" id="totalRequests">-</div>
      <div class="stat-label">Total Requests</div>
    </div>
    <div class="stat-card">
      <div class="stat-value" id="originalBytes">-</div>
      <div class="stat-label">Original Bytes (sent)</div>
    </div>
    <div class="stat-card">
      <div class="stat-value" id="wireBytes">-</div>
      <div class="stat-label">Wire Bytes (actual)</div>
    </div>
    <div class="stat-card">
      <div class="stat-value" id="compressionRatio">-</div>
      <div class="stat-label">Compression Ratio</div>
    </div>
    <div class="stat-card">
      <div class="stat-value" id="responseBytes">-</div>
      <div class="stat-label">Response Bytes (received)</div>
    </div>
  </div>
  <h2 style="color:#00d4ff;margin-bottom:15px;">Recent Requests</h2>
  <table>
    <thead>
      <tr><th>Time</th><th>Method</th><th>URL</th><th>Original</th><th>Wire</th><th>Ratio</th></tr>
    </thead>
    <tbody id="requestsTable"></tbody>
  </table>
  <p class="refresh">Auto-refreshes every 2 seconds</p>
  <script>
    function formatBytes(b) {
      if (b === 0) return '0 B';
      const k = 1024, sizes = ['B', 'KB', 'MB', 'GB'];
      const i = Math.floor(Math.log(b) / Math.log(k));
      return (b / Math.pow(k, i)).toFixed(2) + ' ' + sizes[i];
    }
    function ratioClass(r) {
      const n = parseFloat(r);
      if (n < 30) return 'ratio-good';
      if (n < 70) return 'ratio-ok';
      return 'ratio-bad';
    }
    async function refresh() {
      try {
        const resp = await fetch('/api/stats');
        const data = await resp.json();
        document.getElementById('totalRequests').textContent = data.totalRequests;
        document.getElementById('originalBytes').textContent = formatBytes(data.totalOriginalBytes);
        document.getElementById('wireBytes').textContent = formatBytes(data.totalWireBytes);
        document.getElementById('compressionRatio').textContent = data.compressionRatio + '%';
        document.getElementById('responseBytes').textContent = formatBytes(data.totalResponseBytes);
        const tbody = document.getElementById('requestsTable');
        tbody.innerHTML = data.recentRequests.slice().reverse().map(r => {
          const time = new Date(r.timestamp).toLocaleTimeString();
          const url = r.url.length > 50 ? r.url.substring(0, 50) + '...' : r.url;
          return '<tr><td>' + time + '</td><td>' + r.method + '</td><td>' + url + '</td><td>' + formatBytes(r.originalBytes) + '</td><td>' + formatBytes(r.wireBytes) + '</td><td class="' + ratioClass(r.compressionRatio) + '">' + r.compressionRatio + '%</td></tr>';
        }).join('');
      } catch (e) { console.error('Failed to fetch stats:', e); }
    }
    refresh();
    setInterval(refresh, 2000);
  </script>
</body>
</html>`;

if (STATS_PORT > 0) {
    const statsServer = http.createServer((req, res) => {
        if (req.url === '/api/stats') {
            res.setHeader('Content-Type', 'application/json');
            res.end(JSON.stringify(pipe.getStats()));
        } else if (req.url === '/api/errors' || req.url?.startsWith('/api/errors?')) {
            const url = new URL(req.url, 'http://localhost');
            const n = parseInt(url.searchParams.get('n') || '50', 10);
            const errors = pipe.getErrors(n);
            res.setHeader('Content-Type', 'application/json');
            res.end(JSON.stringify({ count: errors.length, entries: errors }));
        } else if (req.url === '/profile/export') {
            const profiling = pipe.getProfilingData();
            if (!profiling.enabled) {
                res.statusCode = 403;
                res.setHeader('Content-Type', 'application/json');
                res.end(JSON.stringify({ error: 'Profiling not enabled. Start client with PIPE_PROFILE=1' }));
                return;
            }
            res.setHeader('Content-Type', 'application/json');
            res.setHeader('Content-Disposition', 'attachment; filename=stream-relay-profiling-' + new Date().toISOString() + '.json');
            res.end(JSON.stringify(profiling, null, 2));
        } else {
            res.setHeader('Content-Type', 'text/html');
            res.end(statsHtml);
        }
    });

    statsServer.listen(STATS_PORT, () => {
        if (!STREAM_RELAY_QUIET) {
            console.log(`[client] stats dashboard at http://127.0.0.1:${STATS_PORT}`);
        }
    });
} else {
    if (!STREAM_RELAY_QUIET) {
        console.log('[client] stats dashboard disabled (STATS_PORT=0)');
    }
}
