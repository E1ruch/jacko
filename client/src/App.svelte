<script lang="ts">
  import Auth from './Auth.svelte'
  import Workspace from './Workspace.svelte'
  import type { TokenPair } from './lib/api'
  import { authPath, destination, isAuthRoute, parseRoute } from './lib/routes'
  import { Session } from './lib/session'

  let path = $state(location.pathname)
  let search = $state(location.search)
  let authEmail = $state('')
  let authNotice = $state('')
  let session = $state<Session>()
  const route = $derived(parseRoute(path))
  const auth = $derived(isAuthRoute(route) ? route.kind : 'login')
  const next = $derived(isAuthRoute(route)
    ? destination(search)
    : destination(`?next=${encodeURIComponent(path)}`))

  function readLocation() {
    path = location.pathname
    search = location.search
    if (!session && !isAuthRoute(route)) {
      navigate(authPath('login', next), true)
    } else if (session && isAuthRoute(route)) {
      navigate(next, true)
    }
  }

  function navigate(to: string, replace = false) {
    if (location.pathname + location.search === to) return
    if (replace) history.replaceState(null, '', to)
    else history.pushState(null, '', to)
    readLocation()
  }

  function click(event: MouseEvent) {
    if (event.defaultPrevented || event.button !== 0 || event.ctrlKey || event.metaKey || event.shiftKey || event.altKey) return
    if (!(event.target instanceof Element)) return
    const link = event.target.closest('a')
    if (!link || link.target || link.download || link.origin !== location.origin || link.hash) return
    event.preventDefault()
    navigate(link.pathname + link.search)
  }

  function onVerify(email: string) {
    authEmail = email
    navigate(authPath('verify', next), true)
  }

  function onAccountComplete(email: string, notice: string) {
    authEmail = email
    authNotice = notice
    navigate(authPath('login', next), true)
  }

  function onLogin(tokens: TokenPair) {
    const target = next
    authEmail = ''
    authNotice = ''
    session = new Session(tokens, () => {
      session = undefined
      readLocation()
    })
    navigate(target, true)
  }

  $effect(() => {
    readLocation()
  })
</script>

<svelte:window onpopstate={readLocation} onclick={click} />

{#if session}
  <Workspace {route} {session} {navigate} />
{:else}
  {#key auth + next}
    <Auth mode={auth} {next} initialEmail={authEmail} notice={authNotice}
      {onLogin} {onVerify} {onAccountComplete} />
  {/key}
{/if}
