#!/usr/bin/env node
// Test BodyDict compression with realistic LLM conversation patterns

const fs = require('fs');
const path = require('path');
const { createPipeClient } = require('../../client/pipe-client.js');

const relayBase = process.argv[2] || 'http://localhost:29998';
const echoTarget = process.argv[3] || 'http://localhost:3333/echo';
const proxyBase = process.argv[4] || 'http://localhost:9999';

// Fetch server metrics
async function getMetrics() {
  try {
    const resp = await fetch(`${relayBase}/debug/vars`, { method: 'GET' });
    if (!resp.ok) return null;
    return await resp.json();
  } catch (e) {
    return null;
  }
}

// Create a large conversation-like JSON payload with repetition
function createLargeConversation(rounds = 5) {
  const systemPrompt = `You are a helpful AI assistant. You specialize in answering questions about software engineering, system design, and best practices. When asked a question, you provide detailed, well-structured responses with examples and explanations.`;

  const messages = [];

  // Add system message
  messages.push({
    role: 'system',
    content: systemPrompt
  });

  // Add conversation rounds with repetitive context
  for (let i = 0; i < rounds; i++) {
    // User message
    messages.push({
      role: 'user',
      content: `Question ${i + 1}: Can you explain how caching works in distributed systems? I want to understand the different strategies like LRU, LFU, and TTL-based eviction. Also explain how cache invalidation challenges affect system design.`
    });

    // Assistant response (simulated, with repeated patterns)
    const response = `Caching is a critical technique in distributed systems that improves performance by reducing latency and load on backend services.

## Caching Strategies

### LRU (Least Recently Used)
- Evicts the least recently accessed items when cache is full
- Good for temporal locality patterns
- O(1) access time with hash table + doubly linked list

### LFU (Least Frequently Used)
- Tracks access frequency
- Better for skewed access patterns
- Slightly more overhead than LRU

### TTL-based Eviction
- Items automatically expire after time duration
- Simple to implement
- Useful for data that changes periodically

## Cache Invalidation Challenges

The famous quote: "There are only two hard things in Computer Science: cache invalidation and naming things."

Cache invalidation can be solved by:
1. Time-based expiration (TTL)
2. Event-based invalidation (push invalidation)
3. Dependency-based invalidation

## Best Practices

- Use write-through caching for critical data
- Implement cache warming strategies
- Monitor cache hit rates
- Plan for cache misses in your system design
- Use cache stampede prevention techniques

When designing a system, remember that cache is not a replacement for correct data management but rather a performance optimization layer.`;

    messages.push({
      role: 'assistant',
      content: response
    });
  }

  return { messages };
}

async function runTest() {
  console.log(`\n${'='.repeat(60)}`);
  console.log('BodyDict Compression Test');
  console.log(`${'='.repeat(60)}`);
  console.log(`Relay: ${relayBase}`);
  console.log(`Target: ${echoTarget}`);
  console.log(`BodyDict: ${process.env.PIPE_BODYDICT === '1' ? 'ENABLED' : 'DISABLED'}`);
  console.log(`Dict Min Size: ${process.env.PIPE_DICT_MIN_SIZE || '1024'} bytes\n`);

  const client = createPipeClient({ serverBase: relayBase });
  await client.init();

  // Test 1: Small payload (no compression)
  console.log('Test 1: Small payload (no compression expected)');
  const smallPayload = { message: 'hello world' };
  await testRequest(client, smallPayload, 'small');

  // Test 2: Medium payload
  console.log('\nTest 2: Medium conversation (1-2 rounds)');
  const mediumPayload = createLargeConversation(1);
  await testRequest(client, mediumPayload, 'medium');

  // Test 3: Large payload (compression should kick in)
  console.log('\nTest 3: Large conversation (5 rounds with repetition)');
  const largePayload = createLargeConversation(5);
  await testRequest(client, largePayload, 'large');

  // Test 4: Very large payload (extreme compression)
  console.log('\nTest 4: Very large conversation (10 rounds)');
  const veryLargePayload = createLargeConversation(10);
  await testRequest(client, veryLargePayload, 'very-large');

  // Show final metrics
  console.log(`\n${'='.repeat(60)}`);
  console.log('Final Server Metrics');
  console.log(`${'='.repeat(60)}`);
  const finalMetrics = await getMetrics();
  if (finalMetrics) {
    showMetrics(finalMetrics);
  } else {
    console.log('Could not fetch metrics');
  }

  client.stop();

  // Also test via proxy so dashboard shows stats
  await runProxyTests();

  console.log('\n✓ Test completed successfully\n');
}

async function testRequest(client, payload, label) {
  const jsonStr = JSON.stringify(payload);
  const before = {
    label,
    bodySize: jsonStr.length,
    metrics: await getMetrics()
  };

  console.log(`  Body size: ${formatBytes(jsonStr.length)}`);

  try {
    const result = await client.start({
      url: echoTarget,
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: payload,
      onDelta: () => {}, // Stream data
      onDone: (err) => {
        if (err) console.error(`    Error: ${err.message}`);
      }
    });

    await result.ack();
  } catch (e) {
    console.error(`  Error: ${e.message}`);
    return;
  }

  // Wait a moment for server to process
  await new Promise(r => setTimeout(r, 100));

  const after = await getMetrics();
  showCompressionStats(before, after);
}

function showCompressionStats(before, after) {
  if (!before.metrics || !after) {
    console.log('  (metrics not available)');
    return;
  }

  const mBefore = before.metrics;
  const mAfter = after;

  // expvar returns values directly, not wrapped in {value: ...}
  const putChunksDelta = (mAfter.dict_put_chunks_total || 0) - (mBefore.dict_put_chunks_total || 0);
  const putBytesDelta = (mAfter.dict_put_bytes_total || 0) - (mBefore.dict_put_bytes_total || 0);
  const hitChunksDelta = (mAfter.dict_hit_chunks_total || 0) - (mBefore.dict_hit_chunks_total || 0);
  const reconstructedDelta = (mAfter.dict_reconstructed_bytes_total || 0) - (mBefore.dict_reconstructed_bytes_total || 0);

  if (putChunksDelta > 0) {
    const ratio = ((putBytesDelta / reconstructedDelta) * 100).toFixed(1);
    console.log(`  Chunks stored: ${putChunksDelta}, Size: ${formatBytes(putBytesDelta)}`);
    console.log(`  Reconstructed: ${formatBytes(reconstructedDelta)}`);
    console.log(`  Compression: ${ratio}% (1:${(reconstructedDelta / putBytesDelta).toFixed(1)} ratio)`);
  } else if (hitChunksDelta > 0) {
    console.log(`  Chunks reused: ${hitChunksDelta}`);
    console.log(`  Saved bandwidth: ${formatBytes(reconstructedDelta)}`);
  } else {
    console.log(`  No compression (payload too small)`);
  }
}

function showMetrics(metrics) {
  const m = metrics;
  const wireBytes = m.pipe_send_wire_bytes_total || 0;
  const reconstructed = m.dict_reconstructed_bytes_total || 0;

  console.log(`  Total chunks stored:    ${m.dict_put_chunks_total || 0}`);
  console.log(`  Total bytes stored:     ${formatBytes(m.dict_put_bytes_total || 0)}`);
  console.log(`  Total chunks hit:       ${m.dict_hit_chunks_total || 0}`);
  console.log(`  Missing errors:         ${m.dict_missing_errors_total || 0}`);
  console.log(`  Total reconstructed:    ${formatBytes(reconstructed)}`);
  console.log(`  Wire bytes (sent):      ${formatBytes(wireBytes)}`);
  if (reconstructed > 0 && wireBytes > 0) {
    const ratio = ((wireBytes / reconstructed) * 100).toFixed(1);
    console.log(`  Total compression:      ${ratio}% (BodyDict + Brotli combined)`);
  }
}

function formatBytes(bytes) {
  if (bytes === 0) return '0 B';
  const k = 1024;
  const sizes = ['B', 'KB', 'MB'];
  const i = Math.floor(Math.log(bytes) / Math.log(k));
  return (bytes / Math.pow(k, i)).toFixed(2) + ' ' + sizes[i];
}

// Test via client proxy (for dashboard stats)
async function testViaProxy(payload, label) {
  const jsonStr = JSON.stringify(payload);
  console.log(`  Round ${label}: ${formatBytes(jsonStr.length)}`);

  try {
    const resp = await fetch(proxyBase + '/echo', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: jsonStr,
    });
    await resp.text(); // consume response
  } catch (e) {
    console.error(`  Round ${label}: Error - ${e.message}`);
  }
}

// Simulate realistic LLM conversation where each request contains FULL history
function createGrowingConversation(round) {
  const systemPrompt = `You are a helpful AI assistant. You specialize in answering questions about software engineering, system design, and best practices. When asked a question, you provide detailed, well-structured responses with examples and explanations.`;

  const messages = [{ role: 'system', content: systemPrompt }];

  // Each round adds a user message + assistant response to the SAME conversation
  for (let i = 0; i < round; i++) {
    messages.push({
      role: 'user',
      content: `Question ${i + 1}: Can you explain how caching works in distributed systems? I want to understand the different strategies like LRU, LFU, and TTL-based eviction. Also explain how cache invalidation challenges affect system design.`
    });

    messages.push({
      role: 'assistant',
      content: `Caching is a critical technique in distributed systems that improves performance by reducing latency and load on backend services.

## Caching Strategies

### LRU (Least Recently Used)
- Evicts the least recently accessed items when cache is full
- Good for temporal locality patterns
- O(1) access time with hash table + doubly linked list

### LFU (Least Frequently Used)
- Tracks access frequency
- Better for skewed access patterns
- Slightly more overhead than LRU

### TTL-based Eviction
- Items automatically expire after time duration
- Simple to implement
- Useful for data that changes periodically

## Cache Invalidation Challenges

The famous quote: "There are only two hard things in Computer Science: cache invalidation and naming things."

Cache invalidation can be solved by:
1. Time-based expiration (TTL)
2. Event-based invalidation (push invalidation)
3. Dependency-based invalidation

## Best Practices

- Use write-through caching for critical data
- Implement cache warming strategies
- Monitor cache hit rates
- Plan for cache misses in your system design
- Use cache stampede prevention techniques

When designing a system, remember that cache is not a replacement for correct data management but rather a performance optimization layer.`
    });
  }

  return { messages };
}

async function runProxyTests() {
  console.log(`\n${'='.repeat(60)}`);
  console.log('LLM Conversation Simulation (realistic deduplication)');
  console.log(`${'='.repeat(60)}`);
  console.log(`Proxy: ${proxyBase}`);
  console.log('Each request contains FULL conversation history (like real LLM agents)\n');

  // Simulate 10 rounds of conversation - each round includes ALL previous messages
  for (let round = 1; round <= 10; round++) {
    const conversation = createGrowingConversation(round);
    await testViaProxy(conversation, round);
  }

  console.log('\nOpen the stats dashboard to see compression metrics.');
  console.log('After round 2+, BodyDict should show significant deduplication!');
}

runTest().catch(e => {
  console.error('Test failed:', e);
  process.exit(1);
});
