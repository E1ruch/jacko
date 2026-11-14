<script lang="ts">
  import { untrack } from 'svelte'
  import { account, ApiError, errorText } from './lib/api'
  import type { TokenPair } from './lib/api'
  import { Session } from './lib/session'
  import type { AuthMode } from './lib/routes'
  import { authPath } from './lib/routes'

  let { mode, next, initialEmail, notice, onLogin, onVerify, onAccountComplete }: {
    mode: AuthMode
    next: string
    initialEmail: string
    notice: string
    onVerify: (email: string) => void
    onAccountComplete: (email: string, notice: string) => void
    onLogin: (tokens: TokenPair) => void
  } = $props()

  let email = $state(untrack(() => initialEmail))
  let code = $state('')
  let requested = $state(false)
  let requestNotice = $state('')
  let requestError = $state('')
  let requesting = $state(false)
  const recovery = $derived(mode === 'verify' || mode === 'forgot')
  const verify = $derived(mode === 'verify')
  let password = $state('')
  let busy = $state(false)
  let error = $state('')
  let registered = $state(false)
  const scope = new AbortController()
  const registration = $derived(mode === 'register')
  const createAccount = $derived(registration && !registered)

  $effect(() => () => scope.abort())

  async function submit(event: SubmitEvent) {
    event.preventDefault()
    if (busy) return
    busy = true
    error = ''
    const credentials = { email: email.trim(), password }
    try {
      if (createAccount) {
        await Session.register(credentials, scope.signal)
        registered = true
      }
      const tokens = await Session.login(credentials, scope.signal)
      if (!scope.signal.aborted) onLogin(tokens)
    } catch (cause) {
      if (scope.signal.aborted) return
      if (cause instanceof ApiError && cause.code === 'email_not_verified') {
        onVerify(email.trim())
      } else {
        error = errorText(cause)
      }
    } finally {
      busy = false
    }
  }
  function mailError(cause: unknown) {
    return cause instanceof ApiError && cause.status === 404
      ? 'Почтовый режим недоступен на этом сервере.'
      : errorText(cause)
  }

  async function requestCode(event: SubmitEvent) {
    event.preventDefault()
    if (requesting || busy) return
    requesting = true
    requestNotice = ''
    requestError = ''
    try {
      await (verify ? account.resend : account.forgot)(email.trim(), scope.signal)
      if (scope.signal.aborted) return
      requested = true
      requestNotice = 'Если для этого адреса можно выдать код, письмо придёт на указанную почту.'
    } catch (cause) {
      if (!scope.signal.aborted) requestError = mailError(cause)
    } finally {
      requesting = false
    }
  }

  async function submitCode(event: SubmitEvent) {
    event.preventDefault()
    if (busy || requesting) return
    const value = code.trim()
    if (!/^[0-9a-f]{64}$/i.test(value)) {
      error = 'Введите 64 символа кода из письма.'
      return
    }
    busy = true
    error = ''
    try {
      if (verify) await account.verify(value, scope.signal)
      else await account.reset(value, password, scope.signal)
      if (!scope.signal.aborted) onAccountComplete(email.trim(), verify ? 'Почта подтверждена. Войдите в аккаунт.' : 'Пароль изменён. Войдите с новым паролем.')
    } catch (cause) {
      if (!scope.signal.aborted) error = mailError(cause)
    } finally {
      busy = false
    }
  }
</script>

<main class="auth-screen">
  <section class="auth-card" aria-labelledby="auth-title">
    <div class="wordmark"><span class="brand-mark" aria-hidden="true">J</span>Jacko</div>
    <h1 id="auth-title">{verify ? 'Подтверждение почты' : mode === 'forgot' ? 'Восстановление пароля' : createAccount ? 'Создать аккаунт' : 'Войти в Jacko'}</h1>
    {#if recovery}
      {#if verify}<p class="muted">Введите код подтверждения из письма Jacko.</p>{/if}
      <form class="auth-form" onsubmit={requestCode} aria-describedby={requestError ? 'request-error' : undefined}>
        <label>Электронная почта
          <input type="email" bind:value={email} autocomplete="email" maxlength="254" required disabled={busy || requesting} />
        </label>
        {#if requestNotice}<p class="muted" role="status">{requestNotice}</p>{/if}
        {#if requestError}<p class="error" id="request-error" role="alert">{requestError}</p>{/if}
        <button class="secondary" disabled={busy || requesting}>{requesting ? 'Подождите…' : verify || requested ? 'Запросить код повторно' : 'Запросить код'}</button>
      </form>
      <form class="auth-form" onsubmit={submitCode} aria-describedby={error ? 'auth-error' : undefined}>
        <label>Код из письма
          <input type="text" bind:value={code} autocomplete="one-time-code" spellcheck="false" required disabled={busy} />
        </label>
        {#if !verify}
          <label>Новый пароль
            <input type="password" bind:value={password} autocomplete="new-password" minlength="12" maxlength="256" required disabled={busy} />
          </label>
          <p class="hint muted">Минимум 12 символов.</p>
        {/if}
        {#if error}<p class="error" id="auth-error" role="alert">{error}</p>{/if}
        <button class="primary" disabled={busy || requesting}>{verify ? 'Подтвердить почту' : 'Сменить пароль'}</button>
      </form>
      <p class="auth-switch"><a href={authPath('login', next)}>Вернуться ко входу</a></p>
    {:else}
      {#if mode === 'login' && notice}<p class="muted" role="status">{notice}</p>{/if}
      {#if next.startsWith('/invite/')}
        <p class="muted">Войдите или зарегистрируйтесь, чтобы принять приглашение.</p>
      {:else if next !== '/'}
        <p class="muted">После входа откроется выбранный канал или сообщество.</p>
      {/if}
      <form onsubmit={submit} class="auth-form" aria-describedby={error ? 'auth-error' : undefined}>
        <label>Электронная почта
          <input type="email" bind:value={email} autocomplete="email" maxlength="254" required disabled={busy} />
        </label>
        <label>Пароль
          <input type="password" bind:value={password} autocomplete={createAccount ? 'new-password' : 'current-password'} minlength={createAccount ? 12 : 1} maxlength="256" required disabled={busy} />
        </label>
        {#if createAccount}<p class="hint muted">Минимум 12 символов.</p>{/if}
        {#if error}<p class="error" id="auth-error" role="alert">{error}</p>{/if}
        <button class="primary" disabled={busy}>{busy ? 'Подождите…' : createAccount ? 'Зарегистрироваться' : 'Войти'}</button>
      </form>
      <p class="auth-switch">
        {registration ? 'Уже есть аккаунт?' : 'Впервые в Jacko?'}
        <a href={authPath(registration ? 'login' : 'register', next)}>{registration ? 'Войти' : 'Создать аккаунт'}</a>
      </p>
      {#if mode === 'login'}
        <p class="auth-switch"><a href={authPath('forgot', next)}>Забыли пароль?</a></p>
      {/if}
    {/if}
  </section>
</main>

<style>
  .auth-screen {
    min-height: 100dvh;
    display: grid;
    place-items: center;
    padding: 24px 16px;
    background: radial-gradient(ellipse at 20% 0%, var(--brand-100), transparent 60%),
      var(--bg-app);
  }
  .auth-card {
    width: min(100%, 410px);
    padding: clamp(24px, 6vw, 36px);
    border: 1px solid var(--border);
    border-radius: var(--radius-xl);
    background: var(--surface);
    box-shadow: 0 16px 48px #1a524711;
  }
  h1 { margin: 26px 0 8px; }
  .auth-form { display: grid; gap: 16px; margin-top: 24px; }
  label { display: grid; gap: 6px; font-weight: 600; }
  .hint { margin-top: -10px; font-size: 12px; }
  .auth-switch { margin-top: 22px; }
  .auth-switch a { font-weight: 650; margin-left: 4px; }
</style>
