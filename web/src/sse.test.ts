import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readSSEStream } from './sse.ts'
import type { SSEEvent } from './sse.ts'

function createStreamFromChunks(chunks: Uint8Array[]): ReadableStream<Uint8Array> {
  let index = 0
  return new ReadableStream<Uint8Array>({
    pull(controller) {
      if (index < chunks.length) {
        controller.enqueue(chunks[index++])
      } else {
        controller.close()
      }
    },
  })
}

async function collectStream(chunks: Uint8Array[]): Promise<SSEEvent[]> {
  const stream = createStreamFromChunks(chunks)
  const reader = stream.getReader()
  const events: SSEEvent[] = []
  await readSSEStream(reader, (e) => {
    events.push(e)
  })
  return events
}

test('SSE Parser: Coalesce multiple events in a single chunk', async () => {
  const text =
    ': ping\n\n' +
    'data: {"delta": "Hello ", "request_id": "req-123"}\n\n' +
    'data: {"delta": "World! 🌍", "request_id": "req-123"}\n\n' +
    'event: error\ndata: {"error": "something failed"}\n\n' +
    'data: [DONE]\n\n'

  const rawBytes = new TextEncoder().encode(text)
  const events = await collectStream([rawBytes])

  assert.equal(events.length, 4)
  assert.deepEqual(events[0], {
    event: '',
    data: '{"delta": "Hello ", "request_id": "req-123"}',
  })
  assert.deepEqual(events[1], {
    event: '',
    data: '{"delta": "World! 🌍", "request_id": "req-123"}',
  })
  assert.deepEqual(events[2], {
    event: 'error',
    data: '{"error": "something failed"}',
  })
  assert.deepEqual(events[3], {
    event: '',
    data: '[DONE]',
  })
})

test('SSE Parser: Split at every byte boundary including inside multibyte UTF-8', async () => {
  const text =
    ': ping\n\n' +
    'data: {"delta": "Hello 🚀 🌍 ñáéíóú ü", "request_id": "req-456"}\n\n' +
    'event: error\ndata: {"error": "host offline"}\n\n' +
    'data: [DONE]\n\n'

  const rawBytes = new TextEncoder().encode(text)
  const expectedEvents = await collectStream([rawBytes])

  // 1. Test 1-byte chunks (worst-case fragmentation)
  const singleByteChunks: Uint8Array[] = []
  for (let i = 0; i < rawBytes.length; i++) {
    singleByteChunks.push(rawBytes.subarray(i, i + 1))
  }
  const eventsFromSingleBytes = await collectStream(singleByteChunks)
  assert.deepEqual(eventsFromSingleBytes, expectedEvents)

  // 2. Test split at every single byte index into two chunks
  for (let splitIdx = 1; splitIdx < rawBytes.length; splitIdx++) {
    const chunk1 = rawBytes.subarray(0, splitIdx)
    const chunk2 = rawBytes.subarray(splitIdx)
    const events = await collectStream([chunk1, chunk2])
    assert.deepEqual(
      events,
      expectedEvents,
      `Failed when split at byte boundary ${splitIdx}`
    )
  }

  // 3. Test various chunk sizes (2, 3, 5, 7, 11, 16, 32 bytes)
  for (const chunkSize of [2, 3, 5, 7, 11, 16, 32]) {
    const chunks: Uint8Array[] = []
    for (let i = 0; i < rawBytes.length; i += chunkSize) {
      chunks.push(rawBytes.subarray(i, Math.min(i + chunkSize, rawBytes.length)))
    }
    const events = await collectStream(chunks)
    assert.deepEqual(
      events,
      expectedEvents,
      `Failed with chunk size ${chunkSize}`
    )
  }
})

test('SSE Parser: Client accumulated state matches in no-save mode', async () => {
  const frames = [
    'data: {"delta": "Line 1: ", "request_id": "req-789"}\n\n',
    'data: {"delta": "Line 2: 🌟 special chars \u00A9 \u00AE ", "request_id": "req-789"}\n\n',
    'data: {"delta": "End of message", "request_id": "req-789"}\n\n',
    'data: [DONE]\n\n',
  ]

  const fullText = frames.join('')
  const rawBytes = new TextEncoder().encode(fullText)

  // Helper simulating MyChats.tsx message handling
  async function simulateChatHandling(chunks: Uint8Array[]) {
    const stream = createStreamFromChunks(chunks)
    const reader = stream.getReader()
    let accumulated = ''
    let requestId = ''
    let errorMsg = ''
    let done = false

    await readSSEStream(reader, ({ event, data }) => {
      if (event === 'error') {
        errorMsg = data
        return
      }
      if (data === '[DONE]') {
        done = true
        return
      }
      const parsed = JSON.parse(data)
      if (parsed.delta) {
        accumulated += parsed.delta
      }
      if (parsed.request_id) {
        requestId = parsed.request_id
      }
    })

    return { accumulated, requestId, errorMsg, done }
  }

  const baseline = await simulateChatHandling([rawBytes])
  assert.equal(baseline.done, true)
  assert.equal(baseline.requestId, 'req-789')
  assert.equal(
    baseline.accumulated,
    'Line 1: Line 2: 🌟 special chars © ® End of message'
  )

  // Split at every single byte index
  for (let i = 1; i < rawBytes.length; i++) {
    const result = await simulateChatHandling([
      rawBytes.subarray(0, i),
      rawBytes.subarray(i),
    ])
    assert.deepEqual(result, baseline, `Mismatch at split ${i}`)
  }
})
