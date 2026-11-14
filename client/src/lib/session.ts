import { HistoryCache } from './history-cache'
import { ApiError, request } from './api'
import type { AuthorizedRequest, Credentials, TokenPair } from './api'

export class Session {
  readonly history: HistoryCache
  #tokens: TokenPair
  #rotation: Promise<TokenPair> | undefined
  #timer = 0
  #lifetime = new AbortController()
  #listeners = new Set<() => void>()

  constructor(tokens: TokenPair, private onEnd: () => void) {
    this.#tokens = tokens
    this.history = new HistoryCache(tokens.user_id)
    this.#schedule()
  }

  get userId() { return this.#tokens.user_id }

  static register(body: Credentials, signal: AbortSignal) {
    return request('/auth/register', { method: 'POST', body, signal })
  }

  static login(body: Credentials, signal: AbortSignal) {
    return request<TokenPair>('/auth/login', { method: 'POST', body, signal })
  }

  onRotation(listener: () => void) {
    this.#listeners.add(listener)
    return () => { this.#listeners.delete(listener) }
  }

  async accessToken() {
    this.#lifetime.signal.throwIfAborted()
    if (this.#rotation) return (await this.#rotation).access_token
    if (Date.parse(this.#tokens.expires_at) <= Date.now()) {
      return (await this.#refresh()).access_token
    }
    return this.#tokens.access_token
  }

  api: AuthorizedRequest = async <T>(path: string, options: Parameters<AuthorizedRequest>[1] = {}) => {
    const lifetime = this.#lifetime.signal
    const signal = options.signal ? AbortSignal.any([options.signal, lifetime]) : lifetime
    signal.throwIfAborted()
    const token = await this.accessToken()
    try {
      return await request<T>(path, { ...options, token, signal })
    } catch (error) {
      signal.throwIfAborted()
      if (!(error instanceof ApiError) || error.status !== 401) throw error
      const next = token === this.#tokens.access_token ? await this.#refresh() : this.#tokens
      return request<T>(path, { ...options, token: next.access_token, signal })
    }
  }

  async logout() {
    try {
      await this.api('/auth/session', { method: 'DELETE' })
    } finally {
      this.dispose()
    }
  }

  dispose() {
    if (this.#lifetime.signal.aborted) return
    this.#lifetime.abort()
    window.clearTimeout(this.#timer)
    this.#listeners.clear()
    this.history.clear()
    this.onEnd()
  }

  #schedule(delay = Math.max(1_000, Date.parse(this.#tokens.expires_at) - Date.now() - 30_000)) {
    window.clearTimeout(this.#timer)
    this.#timer = window.setTimeout(() => { void this.#refresh().catch(() => {}) }, delay)
  }

  #refresh() {
    if (this.#rotation) return this.#rotation
    const signal = this.#lifetime.signal
    this.#rotation = request<TokenPair>('/auth/refresh', {
      method: 'POST',
      body: { refresh_token: this.#tokens.refresh_token },
      signal,
    }).then((tokens) => {
      signal.throwIfAborted()
      this.#tokens = tokens
      this.#schedule()
      for (const listener of this.#listeners) listener()
      return tokens
    }).catch((error: unknown) => {
      if (!signal.aborted) {
        if (error instanceof ApiError && error.status === 401) this.dispose()
        else this.#schedule(10_000)
      }
      throw error
    }).finally(() => { this.#rotation = undefined })
    return this.#rotation
  }
}
