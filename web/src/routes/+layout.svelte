<script lang="ts">
  import '../app.css';
  import { page } from '$app/state';
  import { api, ApiError, tokenStore } from '#lib/api.ts';
  import type { Health } from '#lib/types.ts';

  let { children } = $props();
  let health = $state<Health | null>(null);
  let needsToken = $state(false);
  let token = $state('');

  $effect(() => {
    api
      .health()
      .then((h) => (health = h))
      .catch(() => (health = null));
    // /api/health is public: probe a protected endpoint to learn whether a token is required.
    api.jobs().catch((e: unknown) => {
      if (e instanceof ApiError && e.status === 401) needsToken = true;
    });
  });

  function saveToken(e: SubmitEvent) {
    e.preventDefault();
    tokenStore.set(token.trim());
    location.reload();
  }

  const links = [
    { href: '/', label: 'Jobs' },
    { href: '/costs', label: 'Costs' }
  ];
</script>

<header>
  <strong>tarea</strong>
  <nav>
    {#each links as l (l.href)}
      <a href={l.href} aria-current={page.url.pathname === l.href ? 'page' : undefined}>{l.label}</a
      >
    {/each}
  </nav>
  <span class="muted mono">{health ? `v${health.version}` : 'offline'}</span>
</header>

<main>
  {#if needsToken}
    <form onsubmit={saveToken}>
      <p>This server requires an access token (<span class="mono">TAREA_TOKEN</span>).</p>
      <input type="password" bind:value={token} placeholder="token" autocomplete="off" required />
      <button type="submit">Unlock</button>
    </form>
  {:else}
    {@render children()}
  {/if}
</main>

<style>
  header {
    display: flex;
    align-items: center;
    gap: 1.2rem;
    padding: 0.7rem 1rem;
    border-bottom: 1px solid var(--border);
    background: var(--panel);
  }
  nav {
    display: flex;
    gap: 1rem;
    flex: 1;
  }
  nav a[aria-current='page'] {
    font-weight: 600;
    text-decoration: underline;
  }
  main {
    max-width: 960px;
    margin: 0 auto;
    padding: 1.2rem 1rem 3rem;
  }
</style>
