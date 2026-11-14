import type { Message } from './api'

export type HistoryPosition = { id: string; offset: number }
type History = {
  messages: Message[]
  cursor: string
  before?: string
  position?: HistoryPosition
}
type Entry = History & {
  user: string
  channel: string
  touched: number
}

const maxChannels = 8
const maxMessages = 200
const maxBytes = 512 * 1024
const storeName = 'channels'
const memory = new Map<string, Entry>()
const encoder = new TextEncoder()
let database: Promise<IDBDatabase> | undefined

function openDatabase() {
  return database ??= new Promise<IDBDatabase>((resolve, reject) => {
    const request = indexedDB.open('jacko-history', 1)
    request.onupgradeneeded = () => {
      const store = request.result.createObjectStore(storeName, { keyPath: ['user', 'channel'] })
      store.createIndex('user', 'user')
    }
    request.onsuccess = () => resolve(request.result)
    request.onerror = () => reject(request.error)
    request.onblocked = () => reject(new Error('History cache unavailable'))
  }).catch((error) => {
    database = undefined
    throw error
  })
}

async function transaction<T>(
  mode: IDBTransactionMode,
  operation: (store: IDBObjectStore, result: (value: T) => void) => void,
): Promise<T | undefined> {
  try {
    const db = await openDatabase()
    return await new Promise<T | undefined>((resolve, reject) => {
      const tx = db.transaction(storeName, mode)
      let value: T | undefined
      tx.oncomplete = () => resolve(value)
      tx.onabort = () => reject(tx.error)
      operation(tx.objectStore(storeName), (result) => { value = result })
    })
  } catch {
    return undefined
  }
}

export class HistoryCache {
  #disposed = false
  constructor(private user: string) {}

  async read(channel: string) {
    const key = this.#key(channel)
    if (this.#disposed) return
    if (memory.has(key)) return memory.get(key)
    const entry = await transaction<Entry>('readonly', (store, result) => {
      const request = store.get([this.user, channel])
      request.onsuccess = () => result(request.result)
    })
    if (this.#disposed || !entry) return
    return memory.get(key) ?? entry
  }

  write(channel: string, history: History) {
    if (this.#disposed) return
    const { messages, cursor, position } = history
    const recent: Message[] = []
    let bytes = 0
    for (let index = messages.length - 1; index >= 0 && recent.length < maxMessages; index -= 1) {
      const message = messages[index]
      bytes += encoder.encode(JSON.stringify(message)).length
      if (bytes > maxBytes) break
      recent.push(message)
    }
    recent.reverse()
    const before = recent.length < messages.length ? recent[0]?.id : history.before
    const entry: Entry = {
      user: this.user,
      channel,
      touched: Date.now(),
      messages: recent,
      cursor,
      before,
      position,
    }
    memory.set(this.#key(channel), entry)
    const account = [...memory.values()].filter(({ user }) => user === this.user)
    if (account.length > maxChannels) {
      account.sort((a, b) => b.touched - a.touched)
      for (const stale of account.slice(maxChannels)) memory.delete(this.#key(stale.channel))
    }
    void transaction<void>('readwrite', (store) => {
      if (this.#disposed) return
      store.put(entry)
      const request = store.index('user').getAll(this.user)
      request.onsuccess = () => {
        const entries: Entry[] = request.result
        entries.sort((a, b) => b.touched - a.touched)
        for (const stale of entries.slice(maxChannels)) store.delete([stale.user, stale.channel])
      }
    })
  }

  remove(channel: string) {
    memory.delete(this.#key(channel))
    void transaction<void>('readwrite', (store) => { store.delete([this.user, channel]) })
  }

  clear() {
    this.#disposed = true
    for (const [key, entry] of memory) {
      if (entry.user === this.user) memory.delete(key)
    }
    void transaction<void>('readwrite', (store) => {
      const request = store.index('user').getAllKeys(this.user)
      request.onsuccess = () => {
        for (const key of request.result) store.delete(key)
      }
    })
  }

  #key(channel: string) {
    return `${this.user}:${channel}`
  }
}
