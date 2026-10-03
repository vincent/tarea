<script lang="ts">
  import { page } from '$app/state';
  import { api, ApiError } from '#lib/api.ts';
  import { formatCost, formatDuration, formatTokens, relativeTime } from '#lib/format.ts';
  import StatusBadge from '#lib/StatusBadge.svelte';
  import type { JobDetail, MemoryView, RunSummary } from '#lib/types.ts';

  let job = $state<JobDetail | null>(null);
  let runs = $state<RunSummary[]>([]);
  let memory = $state<MemoryView | null>(null);
  let error = $state<string | null>(null);
  let notice = $state<string | null>(null);
  let starting = $state(false);

  let name = $derived(page.params.name ?? '');

  async function load(n: string) {
    try {
      [job, runs, memory] = await Promise.all([api.job(n), api.jobRuns(n), api.memory(n)]);
      error = null;
    } catch (e) {
      error = e instanceof ApiError && e.status === 404 ? 'Job not found.' : String(e);
    }
  }

  $effect(() => {
    const n = name;
    void load(n);
    const t = setInterval(() => void load(n), 5000);
    return () => clearInterval(t);
  });

  async function runNow() {
    starting = true;
    notice = null;
    try {
      await api.runNow(name);
      notice = 'Run started.';
      await load(name);
    } catch (e) {
      notice = e instanceof ApiError && e.status === 409 ? 'Already running.' : String(e);
    } finally {
      starting = false;
    }
  }
</script>

<p><a href="/">← Jobs</a></p>

{#if error}
  <p class="error">{error}</p>
{:else if job}
  <div class="head">
    <h1>{job.name}</h1>
    <button class="primary" onclick={runNow} disabled={starting || job.running}>
      {job.running ? 'Running…' : 'Run now'}
    </button>
  </div>
  {#if notice}<p class="muted">{notice}</p>{/if}

  <div class="card meta">
    <div><span class="muted">Schedule</span> <span class="mono">{job.schedule}</span></div>
    <div><span class="muted">Next</span> {relativeTime(job.next_run)}</div>
    <div><span class="muted">Model</span> <span class="mono">{job.model}</span></div>
    <div>
      <span class="muted">Budget</span>
      {formatCost(job.budget_usd)} / {job.max_steps} steps
    </div>
    <div><span class="muted">7d cost</span> {formatCost(job.cost_7d)}</div>
    {#if job.fallbacks.length}
      <div>
        <span class="muted">Fallbacks</span> <span class="mono">{job.fallbacks.join(', ')}</span>
      </div>
    {/if}
  </div>

  <h2>Runs</h2>
  <div class="card scroll-x">
    <table>
      <thead>
        <tr><th>When</th><th>Status</th><th>Took</th><th>Tokens</th><th>Cost</th><th>Result</th></tr
        >
      </thead>
      <tbody>
        {#each runs as r (r.id)}
          <tr>
            <td><a href="/jobs/{job.name}/runs/{r.id}">{relativeTime(r.started_at)}</a></td>
            <td><StatusBadge status={r.status} /></td>
            <td>{formatDuration(r.started_at, r.ended_at)}</td>
            <td>{formatTokens(r.prompt_tokens + r.completion_tokens)}</td>
            <td>{formatCost(r.cost_usd)}</td>
            <td class="preview">{r.error ?? r.preview ?? ''}</td>
          </tr>
        {:else}
          <tr><td colspan="6" class="muted">No runs yet.</td></tr>
        {/each}
      </tbody>
    </table>
  </div>

  <h2>Memory <span class="muted mono">{memory?.file}</span></h2>
  <div class="card">
    {#if memory?.content}<pre class="mono">{memory.content}</pre>{:else}<span class="muted"
        >Empty.</span
      >{/if}
  </div>

  <h2>Configuration</h2>
  <div class="card">
    <h3>Prompt</h3>
    <pre class="mono">{job.prompt}</pre>
    <h3>MCP servers</h3>
    {#each job.mcp as m (m.name)}
      <div class="mono">
        {m.name} · {m.transport} · {m.target} · allow: {m.allow.join(', ')}
        {#if m.header_keys.length}· headers: {m.header_keys.join(', ')}{/if}
        {#if m.env_keys.length}· env: {m.env_keys.join(', ')}{/if}
      </div>
    {:else}
      <span class="muted">None.</span>
    {/each}
    <h3>Sinks</h3>
    {#each job.sinks as s, i (i)}
      <div class="mono">{s.type} ({s.option_keys.join(', ')})</div>
    {/each}
    <p class="muted">Secret values are never shown.</p>
  </div>
{:else}
  <p class="muted">Loading…</p>
{/if}

<style>
  .head {
    display: flex;
    align-items: center;
    justify-content: space-between;
  }
  .meta {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(220px, 1fr));
    gap: 0.4rem 1.5rem;
  }
  h3 {
    font-size: 0.9rem;
    margin: 1rem 0 0.3rem;
    color: var(--muted);
  }
  .preview {
    max-width: 320px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
</style>
