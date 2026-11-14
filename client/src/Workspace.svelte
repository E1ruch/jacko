<script lang="ts">
  import { errorText, communities } from './lib/api'
  import type { Channel, Guild } from './lib/api'
  import { channelPath, guildPath } from './lib/routes'
  import type { Route } from './lib/routes'
  import type { Session } from './lib/session'
  import type { Outgoing } from './lib/chat'
  import Chat from './Chat.svelte'
  import GuildPanel from './GuildPanel.svelte'

  let { route, session, navigate }: {
    route: Route
    session: Session
    navigate: (path: string) => void
  } = $props()

  const api = $derived(communities(session.api))
  const activeGuild = $derived('guild' in route ? route.guild : undefined)
  const activeChannel = $derived('channel' in route ? route.channel : undefined)
  const invite = $derived(route.kind === 'invite' ? route.token : undefined)
  const outbox: Record<string, Outgoing[]> = $state({})
  const drafts: Record<string, string> = $state({})
  const scope = new AbortController()

  let guilds: Guild[] = $state([])
  let guildState = $state<'loading' | 'ready' | 'error'>('loading')
  let guildError = $state('')
  let guildReload = $state(0)
  const guild = $derived(guilds.find((item) => item.id === activeGuild))
  const isOwner = $derived(guild?.owner_id === session.userId)

  let channelPage: { guild?: string; items: Channel[]; error?: string } = $state({ items: [] })
  let channelReload = $state(0)
  const channels = $derived(channelPage.guild === activeGuild ? channelPage.items : [])
  const channel = $derived(channels.find((item) => item.id === activeChannel))
  const channelsLoading = $derived(activeGuild !== undefined && channelPage.guild !== activeGuild)
  const channelsError = $derived(channelPage.guild === activeGuild ? channelPage.error : undefined)
  const unavailableGuild = $derived(Boolean(activeGuild) && guildState === 'ready' && !guild)
  const showMain = $derived(Boolean(activeChannel) || unavailableGuild || route.kind === 'invite' || route.kind === 'missing')

  let createDialog: HTMLDialogElement
  let inviteDialog: HTMLDialogElement
  let name = $state('')
  let createError = $state('')
  let creating = $state(false)
  let inviteLink = $state('')
  let inviteError = $state('')
  let invitationBusy = $state(false)
  let copied = $state(false)
  let redeemError = $state('')
  let redeeming = $state(false)
  let panel = $state<{ kind: 'members' | 'reports'; guild: string }>()
  let signingOut = $state(false)

  $effect(() => () => scope.abort())

  $effect(() => {
    void guildReload
    const controller = new AbortController()
    guildState = 'loading'
    api.list(controller.signal).then((items) => {
      if (controller.signal.aborted) return
      guilds = items
      guildState = 'ready'
    }).catch((error) => {
      if (controller.signal.aborted) return
      guildError = errorText(error)
      guildState = 'error'
    })
    return () => controller.abort()
  })

  $effect(() => {
    void channelReload
    const id = activeGuild
    const controller = new AbortController()
    channelPage = { items: [] }
    if (id && guildState === 'ready' && guild) {
      api.channels(id, controller.signal).then((items) => {
        if (!controller.signal.aborted) channelPage = { guild: id, items }
      }).catch((error) => {
        if (!controller.signal.aborted) channelPage = { guild: id, items: [], error: errorText(error) }
      })
    }
    return () => controller.abort()
  })

  function showCreate() {
    name = ''
    createError = ''
    createDialog.showModal()
  }

  async function create(event: SubmitEvent) {
    event.preventDefault()
    if (creating) return
    creating = true
    createError = ''
    try {
      const result = await api.create(name.trim(), scope.signal)
      if (scope.signal.aborted) return
      guilds = [...guilds, result.guild]
      createDialog.close()
      guildReload += 1
      navigate(channelPath(result.guild.id, result.channel.id))
    } catch (error) {
      if (!scope.signal.aborted) createError = errorText(error)
    } finally {
      creating = false
    }
  }

  async function showInvite() {
    if (!activeGuild || invitationBusy) return
    invitationBusy = true
    inviteLink = ''
    inviteError = ''
    copied = false
    inviteDialog.showModal()
    try {
      const result = await api.invite(activeGuild, scope.signal)
      if (!scope.signal.aborted) inviteLink = new URL(`/invite/${result.token}`, location.origin).href
    } catch (error) {
      if (!scope.signal.aborted) inviteError = errorText(error)
    } finally {
      invitationBusy = false
    }
  }

  async function copyInvite() {
    try {
      await navigator.clipboard.writeText(inviteLink)
      copied = true
    } catch {
      inviteError = 'Выделите ссылку и скопируйте её вручную.'
    }
  }

  async function redeem(token: string) {
    if (redeeming) return
    redeeming = true
    redeemError = ''
    try {
      const joined = await api.redeem(token, scope.signal)
      if (scope.signal.aborted) return
      guildReload += 1
      if (invite === token) navigate(guildPath(joined.id))
    } catch (error) {
      if (!scope.signal.aborted && invite === token) redeemError = errorText(error)
    } finally {
      redeeming = false
    }
  }

  async function logout() {
    signingOut = true
    await session.logout().catch(() => {})
  }

  function initials(label: string) {
    return Array.from(label.trim()).slice(0, 2).join('').toLocaleUpperCase('ru')
  }
</script>

<div class:show-main={showMain} class="workspace">
  <nav class="rail" aria-label="Сообщества">
    <a class="brand-mark" href="/" aria-label="Jacko, главная">J</a>
    <div class="guild-icons">
      {#each guilds as item (item.id)}
        <a class="guild-icon" href={guildPath(item.id)} class:selected={item.id === activeGuild}
          aria-current={item.id === activeGuild ? 'true' : undefined} aria-label={item.name} title={item.name}>
          {initials(item.name)}
        </a>
      {/each}
    </div>
    <button class="guild-icon add" type="button" onclick={showCreate} disabled={guildState === 'loading'}
      aria-label="Создать сообщество" title="Создать сообщество">+</button>
  </nav>

  <aside class="sidebar">
    <header class="sidebar-header"><a class="wordmark" href="/">Jacko</a></header>
    <div class="sidebar-content">
      {#if guildState === 'loading'}
        <p class="muted" role="status">Загружаем сообщества…</p>
      {:else if guildState === 'error'}
        <p class="error" role="alert">{guildError}</p>
        <button class="secondary" onclick={() => guildReload += 1}>Повторить</button>
      {:else if guild}
        <h2>{guild.name}</h2>
        <nav class="links" aria-label="Текстовые каналы">
          <p class="section-label">Текстовые каналы</p>
          {#if channelsLoading}
            <p class="muted" role="status">Загружаем каналы…</p>
          {:else if channelsError}
            <p class="error" role="alert">{channelsError}</p>
            <button class="secondary" onclick={() => channelReload += 1}>Повторить</button>
          {:else if channels.length === 0}
            <p class="muted">В сообществе пока нет каналов.</p>
          {:else}
            {#each channels as item (item.id)}
              <a class="nav-link" class:selected={item.id === activeChannel}
                aria-current={item.id === activeChannel ? 'page' : undefined} href={channelPath(guild.id, item.id)}>
                <span aria-hidden="true">#</span> {item.name}
              </a>
            {/each}
          {/if}
        </nav>
        <div class="guild-actions">
          <button class="secondary" onclick={() => { panel = { kind: 'members', guild: guild.id } }}>Участники</button>
          {#if isOwner}
            <button class="secondary" onclick={showInvite}>Пригласить участника</button>
            <button class="secondary" onclick={() => { panel = { kind: 'reports', guild: guild.id } }}>Жалобы</button>
          {/if}
        </div>
        <a class="back-link" href="/">← Все сообщества</a>
      {:else}
        <nav class="links" aria-label="Мои сообщества">
          <p class="section-label">Мои сообщества</p>
          {#each guilds as item (item.id)}
            <a class="nav-link" href={guildPath(item.id)}><span class="small-guild">{initials(item.name)}</span>{item.name}</a>
          {:else}
            <p class="muted">У вас пока нет сообществ.</p>
          {/each}
        </nav>
      {/if}
    </div>
    <footer class="account">
      <span class="account-avatar" aria-hidden="true">{session.userId.slice(-2)}</span>
      <span class="account-label">Аккаунт</span>
      <button class="quiet" type="button" onclick={logout} disabled={signingOut}>Выйти</button>
    </footer>
  </aside>

  <main class="main-panel">
    {#if channel && guild}
      {#key channel.id}
        <Chat {channel} guildName={guild.name} {session}
          onMembers={() => { panel = { kind: 'members', guild: channel.guild_id } }}
          pending={outbox[channel.id] ?? []}
          setPending={(id, items) => { outbox[id] = items }}
          draft={drafts[channel.id] ?? ''}
          setDraft={(id, value) => { drafts[id] = value }} />
      {/key}
    {:else}
      <header class="panel-header">
        {#if showMain}
          <a class="mobile-back" aria-label="Назад к списку" href={activeGuild && !unavailableGuild ? guildPath(activeGuild) : '/'}>←</a>
        {/if}
        <div><h1>{invite ? 'Приглашение' : activeChannel ? 'Канал' : guild?.name ?? 'Сообщества'}</h1></div>
      </header>
      <section class="panel-placeholder">
        {#if invite}
          <div class="placeholder-icon" aria-hidden="true">↗</div>
          <h2>Присоединиться к сообществу</h2>
          <p class="muted">Примите приглашение, чтобы открыть каналы сообщества.</p>
          {#if redeemError}<p class="error" role="alert">{redeemError}</p>{/if}
          <button class="primary" onclick={() => redeem(invite)} disabled={redeeming}>
            {redeeming ? 'Подождите…' : 'Принять приглашение'}
          </button>
        {:else if route.kind === 'missing'}
          <h2>Страница не найдена</h2>
          <a href="/">Вернуться к сообществам</a>
        {:else if guildState === 'loading' || (guild && channelsLoading)}
          <p class="muted" role="status">Загружаем {activeGuild ? 'каналы' : 'сообщества'}…</p>
        {:else if guildState === 'error'}
          <p class="error" role="alert">{guildError}</p>
          <button class="secondary" onclick={() => guildReload += 1}>Повторить</button>
        {:else if unavailableGuild}
          <h2>Сообщество недоступно</h2>
          <p class="muted">Проверьте ссылку или попросите участника пригласить вас.</p>
          <a href="/">К сообществам</a>
        {:else if channelsError}
          <p class="error" role="alert">{channelsError}</p>
          <button class="secondary" onclick={() => channelReload += 1}>Повторить</button>
        {:else if activeChannel}
          <h2>Канал недоступен</h2>
          <a href={guildPath(activeGuild!)}>К списку каналов</a>
        {:else if guild}
          <div class="placeholder-icon" aria-hidden="true">#</div>
          <h2>{channels.length ? 'Выберите канал' : 'В сообществе пока нет каналов'}</h2>
        {:else}
          <div class="placeholder-icon" aria-hidden="true">J</div>
          <h2>{guilds.length ? 'Выберите сообщество' : 'Создайте первое сообщество'}</h2>
          <p class="muted">{guilds.length ? 'Откройте сообщество в списке, чтобы увидеть его каналы.' : 'Пригласите друзей и начните общаться в канале.'}</p>
          {#if !guilds.length}<button class="primary" onclick={showCreate}>Создать сообщество</button>{/if}
        {/if}
      </section>
    {/if}
  </main>
</div>

<dialog bind:this={createDialog} aria-labelledby="create-title">
  <h2 id="create-title">Новое сообщество</h2>
  <form onsubmit={create}>
    <label>Название <input bind:value={name} maxlength="80" required disabled={creating} /></label>
    {#if createError}<p class="error" role="alert">{createError}</p>{/if}
    <div class="dialog-actions">
      <button class="secondary" type="button" onclick={() => createDialog.close()}>Закрыть</button>
      <button class="primary" disabled={creating || !name.trim()}>{creating ? 'Создаём…' : 'Создать'}</button>
    </div>
  </form>
</dialog>

<dialog bind:this={inviteDialog} aria-labelledby="invite-title">
  <h2 id="invite-title">Пригласить участника</h2>
  {#if invitationBusy}<p class="muted" role="status">Создаём приглашение…</p>{/if}
  {#if inviteLink}
    <label>Ссылка на приглашение
      <input type="text" readonly value={inviteLink} onclick={(event) => event.currentTarget.select()} />
    </label>
    <p class="muted">Ссылка действует семь дней.</p>
  {/if}
  {#if inviteError}<p class="error" role="alert">{inviteError}</p>{/if}
  <div class="dialog-actions">
    <button class="secondary" type="button" onclick={() => inviteDialog.close()}>Закрыть</button>
    {#if inviteLink}<button class="primary" type="button" onclick={copyInvite}>{copied ? 'Скопировано' : 'Копировать'}</button>{/if}
  </div>
</dialog>

{#if panel && panel.guild === activeGuild}
  {#key panel}
    <GuildPanel kind={panel.kind} guild={panel.guild} {session}
      onClose={() => { panel = undefined }}
      onNavigate={(path) => { panel = undefined; navigate(path) }} />
  {/key}
{/if}
