<script lang="ts">
  import { tick, untrack } from 'svelte'
  import { SvelteMap, SvelteSet } from 'svelte/reactivity'
  import { ApiError, errorText, images, messages } from './lib/api'
  import type { Channel, Message } from './lib/api'
  import { newestVersion } from './lib/chat'
  import type { Outgoing } from './lib/chat'
  import type { HistoryPosition } from './lib/history-cache'
  import { rowAt } from './lib/history-layout'
  import { compareIds, subscribe } from './lib/realtime'
  import type { Dispatch } from './lib/realtime'
  import MessageImage from './MessageImage.svelte'
  import { guildPath } from './lib/routes'
  import type { Session } from './lib/session'

  let { channel, guildName, session, onMembers, pending, setPending, draft, setDraft }: {
    channel: Channel
    guildName: string
    session: Session
    onMembers: () => void
    pending: Outgoing[]
    setPending: (channel: string, items: Outgoing[]) => void
    draft: string
    setDraft: (channel: string, value: string) => void
  } = $props()

  const store = new SvelteMap<string, Message>()
  const newerEvents = new Map<string, Message>()
  const deleted = new Set<string>()
  const awaitingResponse = new SvelteSet<string>()
  const lifetime = new AbortController()
  const api = $derived(messages(session.api, channel.id))
  const attachments = $derived(images(session.api, channel.id))
  const user = $derived(session.userId)
  const sending = $derived(pending.some((item) => item.status === 'sending'))
  const ordered = $derived([...store.values()]
    .filter((message) => !sending || !awaitingResponse.has(message.id))
    .sort((a, b) => compareIds(a.id, b.id)))
  let cursor: string | undefined
  let before = $state<string>()
  let historyRequest: AbortController | undefined
  let connectionState = $state<'loading' | 'connected' | 'reconnecting' | 'error'>('loading')
  let loaded = $state(false)
  let loadingOlder = $state(false)
  let olderError = $state('')
  let channelError = $state('')
  let inputError = $state('')
  let replyTo = $state<string>()
  let selectedFile = $state<File>()
  let fileInput: HTMLInputElement
  let messageDialog: HTMLDialogElement
  let action = $state<{ kind: 'edit' | 'delete' | 'report'; message: Message }>()
  let editText = $state('')
  let actionError = $state('')
  let actionBusy = $state(false)
  let actionNotice = $state('')
  const replyTarget = $derived(replyTo ? store.get(replyTo) : undefined)
  let generation = $state(0)
  let scroll: HTMLDivElement
  let list: HTMLOListElement
  let viewport = $state({ top: 0, height: 0 })
  const heights = new SvelteMap<string, number>()
  const days = $derived(ordered.map((message) => day.format(new Date(message.created_at))))
  const offsets = $derived.by(() => {
    const result = [0]
    for (const [index, message] of ordered.entries()) {
      const hasDate = index === 0 || days[index] !== days[index - 1]
      result.push(result[index] + (heights.get(message.id) ?? 64) + (hasDate ? 52 : 0))
    }
    return result
  })
  const height = $derived(offsets[offsets.length - 1])
  const start = $derived(Math.min(ordered.length, rowAt(offsets, Math.max(0, viewport.top - 480))))
  const end = $derived(Math.min(ordered.length, rowAt(offsets, viewport.top + viewport.height + 480) + 1))
  let cacheTimer = 0
  let restore: HistoryPosition | undefined
  let lastPosition: HistoryPosition | undefined
  let canSave = false
  let pendingAnchor: HistoryPosition | undefined
  let layoutScheduled = false
  let composer: HTMLTextAreaElement
  let follow = true

  const time = new Intl.DateTimeFormat('ru', { hour: '2-digit', minute: '2-digit' })
  const day = new Intl.DateTimeFormat('ru', { day: 'numeric', month: 'long', year: 'numeric' })

  $effect(() => {
    const id = channel.id
    return () => {
      saveHistory(id)
      window.clearTimeout(cacheTimer)
      lifetime.abort()
      historyRequest?.abort()
      // A canceled POST may have committed. Keep its nonce for a manual retry.
      const outstanding = untrack(() => pending)
      if (outstanding.some((item) => item.status === 'sending')) {
        setPending(id, outstanding.map((item) => item.status === 'sending'
          ? { ...item, status: 'failed', error: 'Подтверждение не получено.' }
          : item))
      }
    }
  })

  $effect(() => {
    void generation
    const owner = session
    const id = channel.id
    const controller = new AbortController()
    let socket: ReturnType<typeof subscribe> | undefined
    const stopRotation = owner.onRotation(() => socket?.reconnect())
    untrack(() => { void open() })

    async function open() {
      if (!loaded && cursor === undefined) {
        const cached = await owner.history.read(id)
        if (controller.signal.aborted) return
        if (cached) {
          for (const message of cached.messages) store.set(message.id, message)
          cursor = cached.cursor
          before = cached.before
          restore = cached.position
        }
      }
      socket = subscribe({
        channel: id,
        token: () => owner.accessToken(),
        after: () => cursor,
        snapshot: loadSnapshot,
        dispatch: applyBatch,
        connected: () => {
          channelError = ''
          connectionState = 'connected'
          canSave = true
          if (!loaded || restore) {
            loaded = true
            void restoreScroll()
          }
          scheduleSave()
        },
        disconnected: () => { connectionState = loaded ? 'reconnecting' : 'loading' },
        denied: async (signal) => { await api.history(signal) },
        failed: (error) => {
          channelError = errorText(error)
          connectionState = 'error'
          if (error instanceof ApiError && [403, 404].includes(error.status)) {
            clearHistory()
            loaded = false
            canSave = false
            cursor = undefined
            restore = undefined
            lastPosition = undefined
            owner.history.remove(id)
          }
        },
      })
    }

    return () => {
      controller.abort()
      stopRotation()
      socket?.stop()
    }
  })

  function saveHistory(id = channel.id) {
    if (!canSave || cursor === undefined) return
    const position = restore ?? lastPosition
    session.history.write(id, {
      cursor,
      before,
      position,
      messages: [...store.values()].sort((a, b) => compareIds(a.id, b.id)),
    })
  }

  function scheduleSave() {
    if (!canSave || cacheTimer) return
    cacheTimer = window.setTimeout(() => {
      cacheTimer = 0
      saveHistory()
    }, 900)
  }

  async function restoreScroll() {
    const target = restore
    if (target) {
      follow = false
      while (restore === target && before && !store.has(target.id) &&
        (!ordered.length || compareIds(target.id, ordered[0].id) < 0)) {
        const prior = before
        await older()
        if (before === prior) break
      }
    }
    if (lifetime.signal.aborted || restore !== target) return
    restore = undefined
    await updateScroll(target)
    scheduleSave()
  }

  function clearHistory() {
    historyRequest?.abort()
    historyRequest = undefined
    loadingOlder = false
    olderError = ''
    before = undefined
    store.clear()
    newerEvents.clear()
  }

  async function loadSnapshot(snapshotCursor: string, signal: AbortSignal) {
    const page = await api.history(signal)
    signal.throwIfAborted()
    const previous = new Set(store.keys())
    const anchor = follow ? undefined : scrollAnchor()
    const position = restore ?? anchor
    clearHistory()
    for (const message of page.messages) {
      if (message.author_id === user && sending && !previous.has(message.id)) {
        awaitingResponse.add(message.id)
      }
      store.set(message.id, message)
    }
    before = page.next_before
    cursor = snapshotCursor
    loaded = true
    restore = position
    await updateScroll(position)
  }

  function applyBatch(events: Dispatch[]) {
    const anchor = follow ? undefined : scrollAnchor()
    for (const event of events) apply(event)
    if (loaded) void updateScroll(anchor)
    scheduleSave()
  }

  function apply(event: Dispatch) {
    const { d, seq } = event
    if (event.t === 'MESSAGE_DELETE') {
      removeMessage(d.id)
    } else if (event.t === 'MESSAGE_CREATE' || event.t === 'MESSAGE_UPDATE') {
      const message = event.d
      if (!deleted.has(message.id)) {
        // Only the HTTP response can associate a server ID with the optimistic row.
        if (event.t === 'MESSAGE_CREATE' && message.author_id === user && sending && !store.has(message.id)) {
          awaitingResponse.add(message.id)
        }
        if (event.t === 'MESSAGE_CREATE' || store.has(message.id) || !before) {
          const current = store.get(message.id)
          const next = newestVersion(current, message)
          if (current && current.edited_at !== next.edited_at) heights.delete(message.id)
          store.set(message.id, next)
        } else {
          newerEvents.set(message.id, newestVersion(newerEvents.get(message.id), message))
        }
      }
    } else {
      throw new Error('Unsupported message event')
    }
    cursor = seq
  }

  async function older() {
    const pageCursor = before
    if (!pageCursor || loadingOlder) return
    const controller = new AbortController()
    historyRequest = controller
    loadingOlder = true
    olderError = ''
    try {
      const page = await api.history(controller.signal, pageCursor)
      if (controller.signal.aborted) return
      const anchor = scrollAnchor()
      follow = false
      for (const message of page.messages) {
        if (deleted.has(message.id)) continue
        const current = store.get(message.id) ?? newerEvents.get(message.id)
        store.set(message.id, newestVersion(current, message))
        newerEvents.delete(message.id)
      }
      before = page.next_before
      loadingOlder = false
      await updateScroll(anchor)
      scheduleSave()
    } catch (error) {
      if (!controller.signal.aborted) olderError = errorText(error)
    } finally {
      if (historyRequest === controller) {
        historyRequest = undefined
        loadingOlder = false
      }
    }
  }

  function listOffset() {
    return list.getBoundingClientRect().top - scroll.getBoundingClientRect().top + scroll.scrollTop
  }

  function scrollAnchor(): HistoryPosition | undefined {
    if (!scroll || !list || !loaded || !ordered.length) return
    const top = scroll.scrollTop - listOffset()
    const index = Math.min(ordered.length - 1, rowAt(offsets, top))
    return { id: ordered[index].id, offset: offsets[index] - top }
  }

  function syncViewport() {
    if (scroll && list) viewport = { top: scroll.scrollTop - listOffset(), height: scroll.clientHeight }
  }

  function onScroll() {
    syncViewport()
    follow = scroll.scrollHeight - scroll.scrollTop - scroll.clientHeight < 80
    lastPosition = follow ? undefined : scrollAnchor()
    scheduleSave()
  }

  async function updateScroll(anchor?: HistoryPosition) {
    pendingAnchor = anchor ?? pendingAnchor
    if (layoutScheduled) return
    layoutScheduled = true
    await tick()
    layoutScheduled = false
    anchor = pendingAnchor
    pendingAnchor = undefined
    if (!scroll || !list || lifetime.signal.aborted) return
    if (anchor && !follow) {
      let index = ordered.findIndex(({ id }) => id === anchor.id)
      if (index < 0) index = ordered.findIndex(({ id }) => compareIds(id, anchor.id) > 0)
      if (index >= 0) scroll.scrollTop = listOffset() + offsets[index] - anchor.offset
      else if (ordered.length) scroll.scrollTop = scroll.scrollHeight
    } else if (follow) {
      scroll.scrollTop = scroll.scrollHeight
    }
    syncViewport()
    lastPosition = follow ? undefined : scrollAnchor()
  }

  const observer = new ResizeObserver((entries) => {
    const anchor = follow ? undefined : scrollAnchor()
    for (const { target, borderBoxSize } of entries) {
      if (!target.isConnected) continue
      const id = (target as HTMLElement).dataset.row
      const size = borderBoxSize[0]?.blockSize ?? target.getBoundingClientRect().height
      if (id && size && heights.get(id) !== size) heights.set(id, size)
    }
    void updateScroll(anchor)
  })

  function measure(node: HTMLElement, id: string) {
    node.dataset.row = id
    observer.observe(node)
    return { destroy: () => observer.unobserve(node) }
  }

  function observeViewport(node: HTMLElement) {
    let width = node.clientWidth
    const resize = new ResizeObserver(() => {
      const anchor = follow ? undefined : scrollAnchor()
      if (width !== node.clientWidth) {
        width = node.clientWidth
        heights.clear()
        for (const row of node.querySelectorAll<HTMLElement>('[data-row]')) {
          heights.set(row.dataset.row!, row.getBoundingClientRect().height)
        }
      }
      void updateScroll(anchor)
    })
    resize.observe(node)
    return { destroy: () => resize.disconnect() }
  }

  function removeMessage(id: string) {
    deleted.add(id)
    store.delete(id)
    heights.delete(id)
    awaitingResponse.delete(id)
    newerEvents.delete(id)
  }

  function quote(id: string) {
    const target = store.get(id)
    if (target) return target.content || 'Изображение'
    return deleted.has(id) ? 'Сообщение удалено' : 'Исходное сообщение вне загруженной истории'
  }

  function reply(message: Message) {
    replyTo = message.id
    inputError = ''
    composer.focus()
  }

  function selectAttachment() {
    const file = fileInput.files?.[0]
    fileInput.value = ''
    if (!file) return
    if (!['image/jpeg', 'image/png'].includes(file.type) || !file.size || file.size > 8 * 1024 * 1024) {
      inputError = 'Выберите изображение JPEG или PNG размером до 8 МиБ.'
      return
    }
    inputError = ''
    selectedFile = file
    composer.focus()
  }

  function showAction(kind: 'edit' | 'delete' | 'report', message: Message) {
    action = { kind, message }
    editText = kind === 'edit' ? message.content : ''
    actionNotice = ''
    actionError = ''
    messageDialog.showModal()
  }

  async function confirmAction(event: SubmitEvent) {
    event.preventDefault()
    if (actionBusy || !action) return
    const { kind, message } = action
    const content = editText.trim()
    if (kind === 'edit' && ((!content && !message.attachment_id) || new TextEncoder().encode(content).length > 8192)) {
      actionError = content ? 'Сообщение слишком длинное.' : 'Введите текст сообщения.'
      return
    }
    if (kind === 'report' && (!content || content.includes('\0') || new TextEncoder().encode(content).length > 1000)) {
      actionError = 'Введите причину жалобы длиной до 1000 байт.'
      return
    }
    actionBusy = true
    actionError = ''
    const signal = lifetime.signal
    try {
      if (kind === 'report') {
        await api.report(message.id, content, signal)
        if (!signal.aborted) {
          messageDialog.close()
          actionNotice = 'Жалоба отправлена.'
        }
        return
      }
      const updated = kind === 'edit'
        ? await api.edit(message.id, content, signal)
        : await api.remove(message.id, signal)
      if (signal.aborted) return
      const anchor = follow ? undefined : scrollAnchor()
      if (updated) {
        if (!deleted.has(updated.id)) {
          store.set(updated.id, newestVersion(store.get(updated.id), updated))
          heights.delete(updated.id)
        }
      } else {
        removeMessage(message.id)
      }
      messageDialog.close()
      void updateScroll(anchor)
      scheduleSave()
    } catch (error) {
      if (!signal.aborted) actionError = errorText(error)
    } finally {
      actionBusy = false
    }
  }

  function submit(event?: SubmitEvent) {
    event?.preventDefault()
    const content = draft.trim()
    if (!loaded || (!content && !selectedFile)) return
    inputError = ''
    if (new TextEncoder().encode(content).length > 8_192) {
      inputError = 'Сообщение слишком длинное.'
      return
    }
    const item: Outgoing = {
      nonce: crypto.randomUUID(), content, replyTo, file: selectedFile, status: 'sending',
    }
    replyTo = undefined
    selectedFile = undefined
    setDraft(channel.id, '')
    setPending(channel.id, [...pending, item])
    follow = true
    void updateScroll()
    void send(item)
    composer.focus()
  }

  async function send(item: Outgoing) {
    const signal = lifetime.signal
    const id = channel.id
    try {
      let attachmentId = item.attachmentId
      if (item.file && !attachmentId) {
        attachmentId = await attachments.upload(item.file, signal)
        if (signal.aborted) return
        setPending(id, pending.map((entry) => entry.nonce === item.nonce
          ? { ...entry, attachmentId, file: undefined }
          : entry))
      }
      const result = await api.send({
        nonce: item.nonce, content: item.content, reply_to: item.replyTo, attachment_id: attachmentId,
      }, signal)
      if (signal.aborted) return
      awaitingResponse.delete(result.id)
      if (!deleted.has(result.id)) store.set(result.id, newestVersion(store.get(result.id), result))
      setPending(id, pending.filter((entry) => entry.nonce !== item.nonce))
      scheduleSave()
      void updateScroll()
    } catch (error) {
      if (!signal.aborted) {
        setPending(id, pending.map((entry) => entry.nonce === item.nonce
          ? { ...entry, status: 'failed', error: errorText(error) }
          : entry))
      }
    } finally {
      if (!signal.aborted && !sending) {
        awaitingResponse.clear()
        void updateScroll()
      }
    }
  }

  function retry(item: Outgoing) {
    const latest = pending.find((entry) => entry.nonce === item.nonce)
    if (latest?.status !== 'failed') return
    setPending(channel.id, pending.map((entry) => entry.nonce === item.nonce
      ? { ...entry, status: 'sending', error: undefined }
      : entry))
    void send(item)
  }

  function reconnect() {
    channelError = ''
    generation += 1
  }
</script>

<svelte:window onpagehide={() => saveHistory()} />

<header class="panel-header chat-header">
  <a class="mobile-back" href={guildPath(channel.guild_id)} aria-label="К списку каналов">←</a>
  <div class="channel-icon" aria-hidden="true">#</div>
  <div class="channel-heading">
    <h1>{channel.name}</h1>
    <p class="muted">{guildName}</p>
  </div>
  <p class="connection" class:connected={connectionState === 'connected'} role="status">
    <span aria-hidden="true"></span>
    {connectionState === 'connected' ? 'Подключено' : connectionState === 'error' ? 'Связь прервана' : loaded ? 'Восстанавливаем связь…' : 'Подключаемся…'}
  </p>
  <button class="secondary members-trigger" type="button" onclick={onMembers}>Участники</button>
</header>

{#if actionNotice}<p class="action-notice" role="status">{actionNotice}</p>{/if}

{#if channelError}
  <div class="connection-error">
    <p class="error" role="alert">{channelError}</p>
    <button class="secondary" onclick={reconnect}>Переподключиться</button>
  </div>
{/if}

<div class="history" bind:this={scroll} aria-label="История сообщений"
  use:observeViewport onscroll={onScroll}>
  <div class="history-inner">
    {#if before || loadingOlder}
      <div class="history-actions">
        <button class="secondary" disabled={loadingOlder} onclick={older}>
          {loadingOlder ? 'Загружаем…' : 'Загрузить предыдущие сообщения'}
        </button>
        {#if olderError}<p class="error" role="alert">{olderError}</p>{/if}
      </div>
    {/if}
    {#if !loaded && connectionState !== 'error'}
      <p class="muted empty-history" role="status">Загружаем историю…</p>
    {:else if loaded && !ordered.length && !pending.length}
      <div class="empty-history">
        <div class="placeholder-icon" aria-hidden="true">#</div>
        <h2>Начало канала {channel.name}</h2>
        <p class="muted">Отправьте первое сообщение.</p>
      </div>
    {/if}

    <ol class="message-list" aria-label="Сообщения" bind:this={list}>
      {#if loaded && ordered.length}
        <li aria-hidden="true" style:height={`${offsets[start]}px`}></li>
        {#each ordered.slice(start, end) as message, index (message.id)}
          {@const absoluteIndex = start + index}
          {@const dateLabel = days[absoluteIndex]}
          <li data-message={message.id} aria-posinset={absoluteIndex + 1} aria-setsize={ordered.length}>
            {#if absoluteIndex === 0 || dateLabel !== days[absoluteIndex - 1]}
              <div class="date-divider"><span>{dateLabel}</span></div>
            {/if}
            <div class="message" use:measure={message.id} class:own={message.author_id === user}>
              <span class="message-avatar" aria-hidden="true">{message.author_id === user ? 'Я' : message.author_id.slice(-2)}</span>
              <div class="message-body">
                <div class="byline">
                  <strong>{message.author_id === user ? 'Вы' : `Участник ${message.author_id}`}</strong>
                  <time datetime={message.created_at}>{time.format(new Date(message.created_at))}</time>
                  {#if message.edited_at}<span>изменено</span>{/if}
                </div>
                {#if message.reply_to}<blockquote class="reply-quote">{quote(message.reply_to)}</blockquote>{/if}
                {#if message.content}<p>{message.content}</p>{/if}
                {#if message.attachment_id}
                  {@const attachment = message.attachment_id}
                  <MessageImage download={(signal) => attachments.download(attachment, signal)} />
                {/if}
                <div class="message-actions">
                  <button class="quiet" type="button" onclick={() => reply(message)}>Ответить</button>
                  {#if message.author_id === user}
                    <button class="quiet" type="button" onclick={() => showAction('edit', message)}>Изменить</button>
                    <button class="quiet" type="button" onclick={() => showAction('delete', message)}>Удалить</button>
                  {:else}
                    <button class="quiet" type="button" onclick={() => showAction('report', message)}>Пожаловаться</button>
                  {/if}
                </div>
              </div>
            </div>
          </li>
        {/each}
        <li aria-hidden="true" style:height={`${height - offsets[end]}px`}></li>
      {/if}
    </ol>

    {#if loaded && pending.length}
      <ul class="message-list outbox" aria-label="Исходящие сообщения">
        {#each pending as item (item.nonce)}
          <li class="message own pending">
            <span class="message-avatar" aria-hidden="true">Я</span>
            <div class="message-body">
              <div class="byline"><strong>Вы</strong><span>{item.status === 'sending' ? 'Отправляем…' : 'Отправка не подтверждена'}</span></div>
              {#if item.replyTo}<blockquote class="reply-quote">{quote(item.replyTo)}</blockquote>{/if}
              {#if item.content}<p>{item.content}</p>{/if}
              {#if item.file || item.attachmentId}<p class="muted">{item.file?.name ?? 'Изображение'}</p>{/if}
              {#if item.status === 'failed'}
                <p class="error" role="alert">{item.error}</p>
                <div class="pending-actions">
                  <button class="quiet" onclick={() => retry(item)}>Повторить отправку</button>
                  <button class="quiet" onclick={() => setPending(channel.id, pending.filter((entry) => entry.nonce !== item.nonce))}>Скрыть</button>
                </div>
              {/if}
            </div>
          </li>
        {/each}
      </ul>
    {/if}
  </div>
</div>

<form class="composer" onsubmit={submit}>
  {#if inputError}<p class="error" id="message-error" role="alert">{inputError}</p>{/if}
  {#if replyTo}
    <div class="composer-context">
      <span>Ответ: {replyTarget?.content || quote(replyTo)}</span>
      <button class="quiet" type="button" aria-label="Отменить ответ" onclick={() => { replyTo = undefined }}>×</button>
    </div>
  {/if}
  {#if selectedFile}
    <div class="composer-context">
      <span>{selectedFile.name}</span>
      <button class="quiet" type="button" aria-label="Убрать изображение" onclick={() => { selectedFile = undefined }}>×</button>
    </div>
  {/if}
  <div class="composer-row">
    <input type="file" hidden accept="image/jpeg,image/png" bind:this={fileInput} onchange={selectAttachment} />
    <button class="secondary attach" type="button" disabled={!loaded} aria-label="Прикрепить изображение"
      title="Прикрепить изображение" onclick={() => fileInput.click()}>+</button>
    <textarea bind:this={composer} rows="1" value={draft} aria-label="Сообщение"
      aria-describedby={inputError ? 'message-error' : undefined}
      placeholder={`Сообщение в #${channel.name}`} disabled={!loaded}
      oninput={(event) => { setDraft(channel.id, event.currentTarget.value); inputError = '' }}
      onkeydown={(event) => {
        if (event.key === 'Enter' && !event.shiftKey && !event.isComposing) {
          event.preventDefault()
          submit()
        }
      }}></textarea>
    <button class="primary send" type="submit" disabled={!loaded || (!draft.trim() && !selectedFile)} aria-label="Отправить сообщение" title="Отправить сообщение">↑</button>
  </div>
</form>


<dialog bind:this={messageDialog} aria-labelledby="message-action-title"
  oncancel={(event) => { if (actionBusy) event.preventDefault() }}>
  {#if action}
    <h2 id="message-action-title">{action.kind === 'edit' ? 'Изменить сообщение' : action.kind === 'report' ? 'Жалоба на сообщение' : 'Удалить сообщение?'}</h2>
    <form onsubmit={confirmAction}>
      {#if action.kind === 'edit'}
        <label>Текст сообщения<textarea rows="4" bind:value={editText} disabled={actionBusy}></textarea></label>
      {:else if action.kind === 'report'}
        <label>Причина жалобы<textarea rows="4" bind:value={editText} maxlength="1000" required disabled={actionBusy}></textarea></label>
      {:else}
        <p>Сообщение исчезнет из истории канала.</p>
      {/if}
      {#if actionError}<p class="error" role="alert">{actionError}</p>{/if}
      <div class="dialog-actions">
        <button class="secondary" type="button" disabled={actionBusy} onclick={() => messageDialog.close()}>Отмена</button>
        <button class="primary" disabled={actionBusy}>{action.kind === 'edit' ? 'Сохранить' : action.kind === 'report' ? 'Отправить жалобу' : 'Удалить'}</button>
      </div>
    </form>
  {/if}
</dialog>
