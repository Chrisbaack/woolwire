export interface SSEEvent {
  event: string
  data: string
}

export type SSEEventHandler = (event: SSEEvent) => void

export class SSEParser {
  private buffer = ''
  private currentEvent = ''
  private dataLines: string[] = []
  private onEvent: SSEEventHandler

  constructor(onEvent: SSEEventHandler) {
    this.onEvent = onEvent
  }

  feed(chunk: string) {
    this.buffer += chunk
    let newlineIdx: number
    while ((newlineIdx = this.buffer.indexOf('\n')) !== -1) {
      let line = this.buffer.slice(0, newlineIdx)
      this.buffer = this.buffer.slice(newlineIdx + 1)
      if (line.endsWith('\r')) {
        line = line.slice(0, -1)
      }
      this.parseLine(line)
    }
  }

  flush() {
    if (this.buffer.length > 0) {
      let line = this.buffer
      if (line.endsWith('\r')) {
        line = line.slice(0, -1)
      }
      this.buffer = ''
      this.parseLine(line)
    }
    this.dispatch()
  }

  private parseLine(line: string) {
    if (line === '') {
      this.dispatch()
      return
    }
    if (line.startsWith(':')) {
      // Comment frame (e.g. : ping), ignore
      return
    }
    if (line.startsWith('event: ')) {
      this.currentEvent = line.slice(7).trim()
    } else if (line.startsWith('event:')) {
      this.currentEvent = line.slice(6).trim()
    } else if (line.startsWith('data: ')) {
      this.dataLines.push(line.slice(6))
    } else if (line.startsWith('data:')) {
      this.dataLines.push(line.slice(5))
    }
  }

  private dispatch() {
    if (this.currentEvent !== '' || this.dataLines.length > 0) {
      this.onEvent({
        event: this.currentEvent,
        data: this.dataLines.join('\n'),
      })
      this.currentEvent = ''
      this.dataLines = []
    }
  }
}

export async function readSSEStream(
  reader: ReadableStreamDefaultReader<Uint8Array>,
  onEvent: SSEEventHandler
): Promise<void> {
  const decoder = new TextDecoder()
  const parser = new SSEParser(onEvent)

  while (true) {
    const { done, value } = await reader.read()
    if (done) {
      const rest = decoder.decode()
      if (rest) {
        parser.feed(rest)
      }
      parser.flush()
      break
    }
    if (value) {
      const text = decoder.decode(value, { stream: true })
      if (text) {
        parser.feed(text)
      }
    }
  }
}
