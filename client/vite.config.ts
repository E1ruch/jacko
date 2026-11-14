import { svelte } from '@sveltejs/vite-plugin-svelte'
import { defineConfig, loadEnv } from 'vite'

export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, process.cwd(), 'JACKO_')
  const proxy = {
    '/api/v1': {
      target: env.JACKO_API_TARGET || 'http://127.0.0.1:8080',
      ws: true,
    },
  }

  return {
    plugins: [svelte()],
    server: { proxy },
    preview: { proxy },
  }
})
