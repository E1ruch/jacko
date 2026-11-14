<script lang="ts">
  import { errorText } from './lib/api'
  import type { ImageDownload } from './lib/api'

  let { download }: { download: (signal: AbortSignal) => Promise<ImageDownload> } = $props()
  let near = $state(false)
  let image = $state<ImageDownload>()
  let error = $state('')
  let retry = $state(0)
  let renewed = false
  let revision = $state(0)

  function observe(node: HTMLElement) {
    const observer = new IntersectionObserver(([entry]) => {
      if (!entry.isIntersecting) return
      near = true
      observer.disconnect()
    }, { rootMargin: '320px' })
    observer.observe(node)
    return { destroy: () => observer.disconnect() }
  }

  $effect(() => {
    if (!near) return
    void retry
    const controller = new AbortController()
    error = ''
    download(controller.signal).then((result) => {
      if (!controller.signal.aborted) {
        image = result
        revision += 1
      }
    }).catch((cause) => {
      if (!controller.signal.aborted) error = errorText(cause)
    })
    return () => controller.abort()
  })

  function failed() {
    if (!renewed) {
      renewed = true
      retry += 1
    } else {
      error = 'Не удалось загрузить изображение.'
    }
  }

  function tryAgain() {
    renewed = false
    image = undefined
    retry += 1
  }
</script>

<div class="image-attachment" use:observe>
  {#if error}
    <p class="error" role="alert">{error}</p>
    <button type="button" class="quiet" onclick={tryAgain}>Повторить загрузку изображения</button>
  {:else if image}
    {@const width = Math.min(image.width, 440, 420 * image.width / image.height)}
    {#key revision}
      <img src={image.url} alt="Изображение в сообщении" width={image.width} height={image.height}
        style:width={`${width}px`} decoding="async" referrerpolicy="no-referrer" onerror={failed} />
    {/key}
  {:else}
    <span class="muted">Изображение</span>
  {/if}
</div>

<style>
  .image-attachment { margin-top: 6px; }
  img {
    display: block;
    max-width: 100%;
    height: auto;
    border-radius: var(--radius-sm);
    background: var(--subtle);
  }
</style>
