import type { Message } from './api'

export type Dispatch = { seq: string } & (
  | { t: 'MESSAGE_CREATE' | 'MESSAGE_UPDATE'; d: Message }
  | { t: 'MESSAGE_DELETE'; d: { id: string; channel_id: string } }
)
type Frame =
  | ({ v: 1; op: 'dispatch' } & Dispatch)
  | { v: 1; op: 'ready'; cursor: string }
  | { v: 1; op: 'resync_required' }

type Subscription = {
  channel: string
  after: () => string | undefined
  token: () => Promise<string>
  snapshot: (cursor: string, signal: AbortSignal) => Promise<void>
  dispatch: (events: Dispatch[]) => void
  connected: () => void
  disconnected: () => void
  denied: (signal: AbortSignal) => Promise<void>
  failed: (error: unknown) => void
}

const sequence = /^(0|[1-9][0-9]{0,18})$/
const maxBufferedEvents = 1024

export function compareIds(left: string, right: string) {
  return left.length - right.length || (left < right ? -1 : Number(left > right))
}

export function subscribe(options: Subscription) {
  let attempt: AbortController | undefined
  let retryTimer = 0
  let retries = 0
  let forceSnapshot = false
  let stopped = false

  function reconnect(immediate = false) {
    if (stopped) return
    window.clearTimeout(retryTimer)
    attempt?.abort()
    options.disconnected()
    const backoff = Math.min(500 * 2 ** retries, 15_000)
    const delay = immediate ? 0 : backoff * (0.75 + Math.random() * 0.5)
    if (!immediate) retries = Math.min(retries + 1, 5)
    retryTimer = window.setTimeout(() => { void connect() }, delay)
  }

  async function connect() {
    const controller = new AbortController()
    const { signal } = controller
    attempt = controller
    let socket: WebSocket
    try {
      const token = await options.token()
      if (signal.aborted) return
      const url = new URL(`/api/v1/channels/${options.channel}/events`, window.location.origin)
      url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:'
      socket = new WebSocket(url)
      const after = forceSnapshot ? undefined : options.after()
      socket.onopen = () => socket.send(JSON.stringify({ op: 'identify', token, after }))

      let ready = false
      let loading = after === undefined
      let received = after
      let buffered: Dispatch[] = []
      let animation = 0

      function flush() {
        cancelAnimationFrame(animation)
        animation = 0
        if (!buffered.length || signal.aborted) return
        const batch = buffered
        buffered = []
        try {
          options.dispatch(batch)
        } catch (error) {
          controller.abort()
          options.failed(error)
        }
      }
      const handshake = window.setTimeout(() => reconnect(), 16_000)
      signal.addEventListener('abort', () => {
        window.clearTimeout(handshake)
        cancelAnimationFrame(animation)
        socket.onopen = socket.onmessage = socket.onclose = null
        socket.close()
      }, { once: true })

      socket.onmessage = async (message) => {
        try {
          const frame = JSON.parse(message.data) as Frame
          if (frame.v !== 1) throw new Error('Unsupported event version')
          if (frame.op === 'resync_required') {
            if (forceSnapshot) reconnect()
            else {
              forceSnapshot = true
              reconnect(true)
            }
          } else if (frame.op === 'dispatch') {
            if (!received || !sequence.test(frame.seq) || frame.d.channel_id !== options.channel) {
              throw new Error('Invalid channel event')
            }
            if (compareIds(frame.seq, received) <= 0) return
            if (BigInt(frame.seq) !== BigInt(received) + 1n) {
              reconnect()
              return
            }
            received = frame.seq
            buffered.push(frame)
            if (buffered.length >= maxBufferedEvents) {
              if (loading) reconnect()
              else flush()
            } else if (!loading && !animation) {
              animation = requestAnimationFrame(flush)
            }
          } else if (frame.op === 'ready') {
            if (ready || !sequence.test(frame.cursor) || (after !== undefined && frame.cursor !== received)) {
              throw new Error('Invalid channel cursor')
            }
            ready = true
            received = frame.cursor
            window.clearTimeout(handshake)
            if (loading) {
              await options.snapshot(frame.cursor, signal)
              if (signal.aborted) return
              loading = false
            }
            flush()
            if (signal.aborted) return
            forceSnapshot = false
            retries = 0
            options.connected()
          } else {
            throw new Error('Unsupported channel frame')
          }
        } catch (error) {
          if (!signal.aborted) {
            controller.abort()
            options.failed(error)
          }
        }
      }

      socket.onclose = async ({ code }) => {
        if (code === 1008) {
          try {
            await options.denied(signal)
          } catch (error) {
            if (!signal.aborted) {
              controller.abort()
              options.failed(error)
            }
            return
          }
        }
        if (!signal.aborted) reconnect()
      }
    } catch {
      if (!signal.aborted) reconnect()
    }
  }

  function online() { reconnect(true) }

  window.addEventListener('online', online)
  reconnect(true)
  return {
    reconnect: online,
    stop() {
      stopped = true
      window.removeEventListener('online', online)
      window.clearTimeout(retryTimer)
      attempt?.abort()
    },
  }
}
