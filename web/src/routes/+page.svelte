<script lang="ts">
  import { api } from '#lib/api.ts';
  import { formatCost, relativeTime } from '#lib/format.ts';
  import StatusBadge from '#lib/StatusBadge.svelte';
  import type { JobView } from '#lib/types.ts';

  let jobs = $state<JobView[]>([]);
  let error = $state<string | null>(null);
  let loaded = $state(false);

  async function load() {
    try {
      jobs = await api.jobs();
      error = null;
    } catch (e) {
      error = e instanceof Error ? e.message : String(e);
    } finally {
      loaded = true;
    }
  }

  $effect(() => {
    void load();
    const t = setInterval(load, 5000);
    return () => clearInterval(t);
  });

  let total7d = $derived(jobs.reduce((sum, j) => sum + j.cost_7d, 0));
</script>

<h1>Jobs</h1>

{#if error}
  <p class="error">Cannot reach tarea: {error}</p>
{:else if loaded && jobs.length === 0}
  <p class="muted">
    No jobs yet. Add a YAML file to <span class="mono">data/jobs/</span> and run
    <span class="mono">tarea validate</span>.
  </p>
{:else}
  <div class="card scroll-x">
    <table>
      <thead>
        <tr>
          <th>Job</th>
          <th>Schedule</th>
          <th>Next</th>
          <th>Last run</th>
          <th>7d cost</th>
        </tr>
      </thead>
      <tbody>
        {#each jobs as j (j.name)}
          <tr>
            <td>
              <a href="/jobs/{j.name}">{j.name}</a>
              {#if j.running}<StatusBadge status="running" />{/if}
              {#if !j.enabled}<StatusBadge status="disabled" />{/if}
              <div class="muted mono">{j.model}</div>
            </td>
            <td class="mono">{j.schedule}</td>
            <td>{relativeTime(j.next_run)}</td>
            <td>
              {#if j.last_run}
                <StatusBadge status={j.last_run.status} />
                <span class="muted">{relativeTime(j.last_run.started_at)}</span>
              {:else}
                <span class="muted">never</span>
              {/if}
            </td>
            <td>{formatCost(j.cost_7d)}</td>
          </tr>
        {/each}
      </tbody>
    </table>
  </div>
  <p class="muted">Total last 7 days: {formatCost(total7d)}</p>
{/if}
