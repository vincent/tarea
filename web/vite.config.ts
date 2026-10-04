import adapter from '@sveltejs/adapter-static';
import { vitePreprocess } from '@sveltejs/vite-plugin-svelte';
import { sveltekit } from '@sveltejs/kit/vite';
import { defineConfig } from 'vitest/config';

export default defineConfig({
  plugins: [
    sveltekit({
      preprocess: vitePreprocess(),
      // Pure client-side app served by the Go binary: unknown paths fall back to index.html.
      adapter: adapter({ pages: 'build', assets: 'build', fallback: 'index.html', strict: false })
    })
  ],
  server: {
    // `make dev-api` runs the Go server on :8080.
    proxy: { '/api': 'http://127.0.0.1:8080' }
  },
  test: {
    include: ['src/**/*.test.ts'],
    environment: 'node'
  }
});
