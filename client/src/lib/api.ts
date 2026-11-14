import type { components, operations } from './schema'

export type Guild = components['schemas']['Guild']
export type Channel = components['schemas']['Channel']
export type Message = components['schemas']['Message']
export type TokenPair = components['schemas']['TokenPair']
export type Credentials = components['requestBodies']['Credentials']['content']['application/json']
export type GuildMember = components['schemas']['GuildMember']
export type MemberPage = components['schemas']['MemberPage']
export type MessageReport = components['schemas']['MessageReport']
export type Resolution = NonNullable<MessageReport['resolution']>
export type ImageDownload = Result<'getImageAttachment', 200>
type MediaType = components['schemas']['ImageAttachment']['media_type']
export type SendMessage = operations['sendMessage']['requestBody']['content']['application/json']
type Result<Operation extends keyof operations, Status extends keyof operations[Operation]['responses']> =
  operations[Operation]['responses'][Status] extends { content: { 'application/json': infer Body } }
    ? Body
    : never

export class ApiError extends Error {
  constructor(readonly status: number, readonly code: string) {
    super(code)
  }
}

export async function request<T>(
  path: string,
  options: { method?: string; body?: object; token?: string; signal?: AbortSignal } = {},
): Promise<T> {
  const { method, body, token, signal } = options
  const headers = new Headers({ Accept: 'application/json' })
  if (token) headers.set('Authorization', `Bearer ${token}`)
  if (body) headers.set('Content-Type', 'application/json')
  const timeout = AbortSignal.timeout(15_000)
  const response = await fetch(`/api/v1${path}`, {
    method,
    headers,
    body: body && JSON.stringify(body),
    signal: signal ? AbortSignal.any([signal, timeout]) : timeout,
    cache: 'no-store',
  })
  if (!response.ok) {
    const payload = await response.json().catch(() => null)
    throw new ApiError(response.status, payload?.code ?? 'api_error')
  }
  if (response.status === 202 || response.status === 204) return undefined as T
  return response.json()
}

export type AuthorizedRequest = <T>(path: string, options?: {
  method?: string; body?: object; signal?: AbortSignal
}) => Promise<T>

export function communities(api: AuthorizedRequest) {
  return {
    list: (signal: AbortSignal) =>
      listPages((query) => api<Result<'listGuilds', 200>>(`/guilds${query}`, { signal })),
    create: (name: string, signal: AbortSignal) =>
      api<Result<'createGuild', 201>>('/guilds', { method: 'POST', body: { name }, signal }),
    channels: (guild: string, signal: AbortSignal) =>
      listPages((query) => api<Result<'listChannels', 200>>(`/guilds/${guild}/channels${query}`, { signal })),
    invite: (guild: string, signal: AbortSignal) =>
      api<Result<'createInvite', 201>>(`/guilds/${guild}/invites`, { method: 'POST', signal }),
    members: (guild: string, signal: AbortSignal, after?: string) =>
      api<MemberPage>(`/guilds/${guild}/members${after ? `?after=${after}` : ''}`, { signal }),
    reports: (guild: string, signal: AbortSignal, after?: string) =>
      api<Result<'listGuildReports', 200>>(`/guilds/${guild}/reports${after ? `?after=${after}` : ''}`, { signal }),
    resolve: (guild: string, report: string, resolution: Resolution, signal: AbortSignal) =>
      api<void>(`/guilds/${guild}/reports/${report}`, { method: 'PATCH', body: { resolution }, signal }),
    redeem: (token: string, signal: AbortSignal) =>
      api<Result<'redeemInvite', 200>>('/invites/redeem', { method: 'POST', body: { token }, signal }),
  }
}

async function listPages<T>(
  fetchPage: (query: string) => Promise<{ items: T[]; next_after?: string }>,
): Promise<T[]> {
  const items: T[] = []
  let after: string | undefined
  do {
    const page = await fetchPage(after ? `?after=${after}` : '')
    items.push(...page.items)
    after = page.next_after
  } while (after)
  return items
}

export function messages(api: AuthorizedRequest, channel: string) {
  const path = `/channels/${channel}/messages`
  return {
    history: (signal: AbortSignal, before?: string) =>
      api<Result<'listMessages', 200>>(before ? `${path}?before=${before}` : path, { signal }),
    send: (body: SendMessage, signal: AbortSignal) =>
      api<Result<'sendMessage', 200>>(path, { method: 'POST', body, signal }),
    edit: (id: string, content: string, signal: AbortSignal) =>
      api<Result<'editMessage', 200>>(`${path}/${id}`, { method: 'PATCH', body: { content }, signal }),
    report: (id: string, reason: string, signal: AbortSignal) =>
      api<void>(`${path}/${id}/reports`, { method: 'POST', body: { reason }, signal }),
    remove: (id: string, signal: AbortSignal) =>
      api<void>(`${path}/${id}`, { method: 'DELETE', signal }),
  }
}

export function images(api: AuthorizedRequest, channel: string) {
  return {
    async upload(file: File, signal: AbortSignal) {
      const upload = await api<Result<'beginImageUpload', 201>>(`/channels/${channel}/attachments`, {
        method: 'POST', body: { media_type: file.type as MediaType, size_bytes: file.size }, signal,
      })
      const form = new FormData()
      for (const [key, value] of Object.entries(upload.fields)) form.append(key, value)
      form.append('file', file)
      const response = await fetch(upload.url, {
        method: 'POST',
        body: form,
        credentials: 'omit',
        redirect: 'error',
        referrerPolicy: 'no-referrer',
        signal: AbortSignal.any([signal, AbortSignal.timeout(60_000)]),
      })
      if (!response.ok) throw new ApiError(response.status, 'upload_failed')
      await api<Result<'completeImageUpload', 200>>(`/attachments/${upload.id}/complete`, { method: 'POST', signal })
      return upload.id
    },
    download: (id: string, signal: AbortSignal) =>
      api<ImageDownload>(`/attachments/${id}`, { signal }),
  }
}

export const account = {
  resend: (email: string, signal: AbortSignal) =>
    request<void>('/auth/email/resend', { method: 'POST', body: { email }, signal }),
  verify: (code: string, signal: AbortSignal) =>
    request<void>('/auth/email/verify', { method: 'POST', body: { code }, signal }),
  forgot: (email: string, signal: AbortSignal) =>
    request<void>('/auth/password/forgot', { method: 'POST', body: { email }, signal }),
  reset: (code: string, password: string, signal: AbortSignal) =>
    request<void>('/auth/password/reset', { method: 'POST', body: { code, password }, signal }),
}

const errors: Record<string, string> = {
  invalid_credentials: 'Проверьте адрес почты и пароль.',
  invalid_account_code: 'Код недействителен или срок его действия истёк.',
  invalid_report_reason: 'Введите причину жалобы длиной до 1000 байт.',
  invalid_account: 'Проверьте адрес почты и длину пароля.',
  email_already_registered: 'Этот адрес уже зарегистрирован. Войдите в аккаунт.',
  email_not_verified: 'Подтвердите адрес почты перед входом.',
  invalid_name: 'Проверьте название сообщества.',
  reply_target_not_found: 'Сообщение, на которое вы отвечаете, больше недоступно.',
  message_not_found: 'Сообщение больше недоступно.',
  attachment_not_found: 'Изображение недоступно.',
  invalid_attachment: 'Выберите изображение JPEG или PNG размером до 8 МиБ.',
  invalid_upload: 'Не удалось проверить изображение. Допустимы JPEG и PNG до 4096 пикселей по каждой стороне и до 12 мегапикселей.',
  upload_failed: 'Хранилище не приняло изображение. Попробуйте ещё раз.',
  invalid_message: 'Проверьте текст сообщения.',
  invalid_session: 'Сессия завершилась. Войдите снова.',
  nonce_conflict: 'Не удалось подтвердить отправку этого сообщения.',
}

export function errorText(error: unknown): string {
  if (error instanceof ApiError) {
    if (errors[error.code]) return errors[error.code]
    if (error.status === 403 || error.status === 404) return 'Ссылка недействительна или у вас нет доступа.'
    if (error.status === 429) return 'Слишком много запросов. Попробуйте чуть позже.'
    if (error.status >= 500) return 'Сервер временно недоступен. Попробуйте ещё раз.'
    return 'Не удалось выполнить действие. Попробуйте ещё раз.'
  }
  return 'Не удалось связаться с сервером. Проверьте подключение.'
}
