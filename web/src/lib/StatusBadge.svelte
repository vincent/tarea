<script lang="ts">
  import type { RunStatus } from './types';

  let { status }: { status: RunStatus | 'running' | 'disabled' } = $props();
</script>

<span class="badge {status}">{status}</span>

<style>
  .badge {
    display: inline-block;
    padding: 0.05rem 0.5rem;
    border-radius: 999px;
    font-size: 0.78rem;
    border: 1px solid currentColor;
  }
  .ok {
    color: var(--ok);
  }
  .partial,
  .running {
    color: var(--warn);
  }
  .error {
    color: var(--err);
  }
  .disabled {
    color: var(--muted);
  }
</style>
