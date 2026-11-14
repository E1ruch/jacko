export type AuthMode = 'login' | 'register' | 'verify' | 'forgot'

export type Route =
  | { kind: 'home' | 'missing' }
  | { kind: AuthMode }
  | { kind: 'invite'; token: string }
  | { kind: 'guild'; guild: string }
  | { kind: 'channel'; guild: string; channel: string }

const identifier = /^[1-9][0-9]{0,18}$/
const inviteToken = /^[0-9a-f]{64}$/i

export function parseRoute(path: string): Route {
  if (!path.startsWith('/') || path.startsWith('//')) return { kind: 'missing' }
  const parts = path.replace(/\/$/, '').split('/').slice(1)
  const [segment, id, nested, channel] = parts
  if (parts.length === 0) return { kind: 'home' }
  if (parts.length === 1 && (segment === 'login' || segment === 'register' || segment === 'verify' || segment === 'forgot')) {
    return { kind: segment }
  }
  if (parts.length === 2 && segment === 'invite' && inviteToken.test(id)) {
    return { kind: 'invite', token: id }
  }
  if (segment === 'guilds' && identifier.test(id)) {
    if (parts.length === 2) return { kind: 'guild', guild: id }
    if (parts.length === 4 && nested === 'channels' && identifier.test(channel)) {
      return { kind: 'channel', guild: id, channel }
    }
  }
  return { kind: 'missing' }
}

export function guildPath(id: string) {
  return `/guilds/${id}`
}

export function channelPath(guild: string, channel: string) {
  return `${guildPath(guild)}/channels/${channel}`
}

export function destination(search: string) {
  const path = new URLSearchParams(search).get('next') || '/'
  const route = parseRoute(path)
  return ['guild', 'channel', 'invite'].includes(route.kind) ? path : '/'
}

export function authPath(mode: AuthMode, next: string) {
  return next === '/' ? `/${mode}` : `/${mode}?next=${encodeURIComponent(next)}`
}

export function isAuthRoute(route: Route): route is { kind: AuthMode } {
  return route.kind === 'login' || route.kind === 'register' || route.kind === 'verify' || route.kind === 'forgot'
}
