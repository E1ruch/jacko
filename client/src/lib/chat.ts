import type { Message } from './api'

export type Outgoing = {
  nonce: string
  content: string
  replyTo?: string
  file?: File
  attachmentId?: string
  status: 'sending' | 'failed'
  error?: string
}

export function newestVersion(current: Message | undefined, next: Message): Message {
  if (!current || (next.edited_at ?? '') === (current.edited_at ?? '')) return next
  if (!current.edited_at) return next
  if (!next.edited_at) return current

  const millis = Date.parse(next.edited_at) - Date.parse(current.edited_at)
  if (millis !== 0) return millis > 0 ? next : current

  // Date.parse drops the timestamp's sub-millisecond precision.
  const fraction = (value: string) => (value.match(/\.(\d+)/)?.[1] ?? '').padEnd(9, '0')
  return fraction(next.edited_at) >= fraction(current.edited_at) ? next : current
}
