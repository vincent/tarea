<script lang="ts">
  import { page } from '$app/state';
  import { api, ApiError } from '#lib/api.ts';
  import { formatCost, formatDuration, formatTokens } from '#lib/format.ts';
  import StatusBadge from '#lib/StatusBadge.svelte';
  import type { RunRecord } from '#lib/types.ts';

  let run = $state<RunRecord | null>(null);
  let error = $state<string | null>(null);

  let name = $derived(page.params.name ?? '');
  let id = $derived(page.params.id ?? '');

  $effect(() => {
    const [n, i] = [name, id];
    api
      .run(n, i)
      .then((r) => {
        run = r;
        error = null;
      })
      .catch((e) => {
        error = e instanceof ApiError && e.status === 404 ? 'Run not found.' : String(e);
      });
  });
</script>

<p><a href="/jobs/{name}">← {name}</a></p>

{#if error}
  <p class="error">{error}</p>
{:else if run}
  <h1>Run <span class="mono">{run.id}</span></h1>
  <div class="card meta">
    <StatusBadge status={run.status} />
    <span>{run.trigger}</span>
    <span>{formatDuration(run.started_at, run.ended_at)}</span>
    <span>{run.steps} steps · {run.tool_calls} tool calls</span>
    <span>{formatTokens(run.prompt_tokens)} in / {formatTokens(run.completion_tokens)} out</span>
    <span>{formatCost(run.cost_usd)}</span>
    <span class="mono muted">{run.model}</span>
    {#if run.stop && run.stop !== 'done'}<span class="error">stopped: {run.stop}</span>{/if}
    <span class="muted">{run.delivered ? 'delivered' : 'not delivered'}</span>
  </div>

  {#if run.error}<p class="error">{run.error}</p>{/if}

  <h2>Result</h2>
  <div class="card"><pre>{run.output || '(empty)'}</pre></div>

  <h2>Transcript</h2>
  {#each run.messages ?? [] as m, i (i)}
    <details class="card msg" open={m.role === 'assistant' && !m.tool_calls?.length}>
      <summary>
        <strong>{m.role}</strong>
        {#if m.tool_calls?.length}
          <span class="mono muted">→ {m.tool_calls.map((c) => c.name).join(', ')}</span>
        {/if}
      </summary>
      {#if m.content}<pre class="mono">{m.content}</pre>{/if}
      {#each m.tool_calls ?? [] as c (c.id)}
        <pre class="mono call">{c.name}({c.arguments})</pre>
      {/each}
    </details>
  {/each}
{:else}
  <p class="muted">Loading…</p>
{/if}

<style>
  .meta {
    display: flex;
    flex-wrap: wrap;
    gap: 0.5rem 1.2rem;
    align-items: center;
  }
  .msg {
    margin-bottom: 0.5rem;
  }
  summary {
    cursor: pointer;
  }
  .call {
    margin-top: 0.4rem;
    color: var(--accent);
  }
</style>
