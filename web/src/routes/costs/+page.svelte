<script lang="ts">
  import { api } from '#lib/api.ts';
  import { costByDay, costByJob, formatCost } from '#lib/format.ts';
  import type { RunSummary } from '#lib/types.ts';

  const DAYS = 30;

  let runs = $state<RunSummary[]>([]);
  let error = $state<string | null>(null);

  $effect(() => {
    api
      .allRuns(500)
      .then((r) => (runs = r))
      .catch((e) => (error = String(e)));
  });

  let daily = $derived(costByDay(runs, DAYS));
  let max = $derived(Math.max(...daily.map((d) => d.cost), 0.0001));
  let perJob = $derived(costByJob(runs, daily[0]?.day ?? ''));
  let total = $derived(daily.reduce((s, d) => s + d.cost, 0));
</script>

<h1>Costs <span class="muted">last {DAYS} days</span></h1>

{#if error}
  <p class="error">{error}</p>
{:else}
  <div class="card">
    <p><strong>{formatCost(total)}</strong> <span class="muted">total</span></p>
    <div class="bars" role="img" aria-label="Cost per day">
      {#each daily as d (d.day)}
        <div
          class="bar"
          title="{d.day}: {formatCost(d.cost)}"
          style="height: {(d.cost / max) * 100}%"
        ></div>
      {/each}
    </div>
  </div>

  <h2>By job</h2>
  <div class="card scroll-x">
    <table>
      <thead><tr><th>Job</th><th>Runs</th><th>Cost</th></tr></thead>
      <tbody>
        {#each perJob as t (t.job)}
          <tr
            ><td><a href="/jobs/{t.job}">{t.job}</a></td><td>{t.runs}</td><td
              >{formatCost(t.cost)}</td
            ></tr
          >
        {:else}
          <tr><td colspan="3" class="muted">No runs yet.</td></tr>
        {/each}
      </tbody>
    </table>
  </div>
{/if}

<style>
  .bars {
    display: flex;
    align-items: flex-end;
    gap: 3px;
    height: 120px;
  }
  .bar {
    flex: 1;
    min-height: 2px;
    background: var(--accent);
    border-radius: 2px 2px 0 0;
    opacity: 0.85;
  }
</style>
