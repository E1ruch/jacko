<script lang="ts">
  import { untrack } from 'svelte'
  import { communities, errorText } from './lib/api'
  import type { GuildMember, MessageReport, Resolution } from './lib/api'
  import { channelPath } from './lib/routes'
  import type { Session } from './lib/session'

  let { kind, guild, session, onClose, onNavigate }: {
    kind: 'members' | 'reports'
    guild: string
    session: Session
    onClose: () => void
    onNavigate: (path: string) => void
  } = $props()

  const api = $derived(communities(session.api))
  const scope = new AbortController()
  let dialog: HTMLDialogElement
  let members: (GuildMember & { presenceKnown: boolean })[] = $state([])
  let reports: MessageReport[] = $state([])
  let next = $state<string>()
  let busy = $state(false)
  let error = $state('')
  let ready = $state(false)
  let presenceAvailable = $state(true)
  let activeReport = $state<string>()
  let reportError = $state('')

  $effect(() => {
    dialog.showModal()
    untrack(() => { void load() })
    return () => scope.abort()
  })

  async function load(after?: string) {
    if (busy) return
    busy = true
    error = ''
    try {
      if (kind === 'members') {
        const page = await api.members(guild, scope.signal, after)
        if (scope.signal.aborted) return
        const items = page.items.map((member) => ({ ...member, presenceKnown: page.presence_available }))
        members = after ? [...members, ...items] : items
        presenceAvailable = page.presence_available && (!after || presenceAvailable)
        next = page.next_after
      } else {
        const page = await api.reports(guild, scope.signal, after)
        if (scope.signal.aborted) return
        reports = after ? [...reports, ...page.items] : page.items
        next = page.next_after
      }
      ready = true
    } catch (cause) {
      if (!scope.signal.aborted) error = errorText(cause)
    } finally {
      busy = false
    }
  }

  async function resolve(report: MessageReport, resolution: Resolution) {
    if (activeReport || busy) return
    activeReport = report.id
    reportError = ''
    try {
      await api.resolve(guild, report.id, resolution, scope.signal)
      if (scope.signal.aborted) return
      reports = reports.map((item) => item.id === report.id ? { ...item, resolution } : item)
    } catch (cause) {
      if (!scope.signal.aborted) reportError = errorText(cause)
    } finally {
      activeReport = undefined
    }
  }

  const date = new Intl.DateTimeFormat('ru', { dateStyle: 'short', timeStyle: 'short' })
</script>

<dialog class="guild-panel" bind:this={dialog} aria-labelledby="guild-panel-title" onclose={onClose}>
  <header>
    <h2 id="guild-panel-title">{kind === 'members' ? 'Участники' : 'Жалобы'}</h2>
    <button class="quiet" type="button" onclick={() => dialog.close()}>Закрыть</button>
  </header>
  <div class="guild-panel-content">
    {#if busy}<p role="status" class="muted">Загружаем…</p>{/if}
    {#if error}<p role="alert" class="error">{error}</p>{/if}
    {#if kind === 'members'}
      {#if !presenceAvailable}<p class="muted" role="status">Часть онлайн-статусов недоступна.</p>{/if}
      {#if ready && !members.length}<p class="muted">Участников пока нет.</p>{/if}
      <ul class="panel-list">
        {#each members as member (member.user_id)}
          <li>
            <strong>{member.user_id === session.userId ? 'Вы' : `Участник ${member.user_id}`}</strong>
            <p class="muted">{member.role === 'owner' ? 'Владелец' : 'Участник'} ·
              {member.presenceKnown && member.online !== undefined ? (member.online ? 'В сети' : 'Не в сети') : 'Статус неизвестен'}
            </p>
          </li>
        {/each}
      </ul>
    {:else}
      {#if ready && !reports.length}<p class="muted">Жалоб пока нет.</p>{/if}
      {#if reportError}<p role="alert" class="error">{reportError}</p>{/if}
      <ul class="panel-list">
        {#each reports as report (report.id)}
          <li>
            <strong>Сообщение №{report.message_id}</strong>
            <p class="muted">Участник {report.reporter_id} · {date.format(new Date(report.created_at))}</p>
            <p class="report-reason">{report.reason}</p>
            <button class="quiet" type="button" onclick={() => onNavigate(channelPath(guild, report.channel_id))}>Открыть канал</button>
            {#if report.resolution}
              <p class="muted" role="status">{report.resolution === 'reviewed' ? 'Рассмотрена' : 'Отклонена'}</p>
            {:else}
              <div class="panel-actions">
                <button class="secondary" type="button" disabled={Boolean(activeReport) || busy} onclick={() => resolve(report, 'reviewed')}>Отметить рассмотренной</button>
                <button class="quiet" type="button" disabled={Boolean(activeReport) || busy} onclick={() => resolve(report, 'dismissed')}>Отклонить</button>
              </div>
            {/if}
          </li>
        {/each}
      </ul>
    {/if}
    <div class="panel-actions">
      <button class="secondary" type="button" disabled={busy || Boolean(activeReport)} onclick={() => load()}>Обновить</button>
      {#if next}
        <button class="secondary" type="button" disabled={busy || Boolean(activeReport)} onclick={() => load(next)}>Загрузить ещё</button>
      {/if}
    </div>
  </div>
</dialog>

<style>
  .guild-panel { width: min(500px, calc(100% - 32px)); max-height: calc(100dvh - 32px); }
  header { display: flex; align-items: center; justify-content: space-between; gap: 8px; }
  header h2 { margin: 0; }
  .guild-panel-content { margin-top: 12px; }
  .panel-list { list-style: none; margin: 0; padding: 0; }
  .panel-list li { padding: 12px 0; border-bottom: 1px solid var(--border); }
  .panel-list p { margin-top: 4px; }
  .panel-actions { display: flex; flex-wrap: wrap; gap: 6px; margin-top: 8px; }
  .report-reason { white-space: pre-wrap; overflow-wrap: anywhere; }
</style>
