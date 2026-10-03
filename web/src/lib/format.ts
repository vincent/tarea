export function formatCost(usd: number): string {
  if (usd === 0) return '$0';
  if (usd < 0.01) return `$${usd.toFixed(4)}`;
  return `$${usd.toFixed(2)}`;
}

export function formatDuration(startISO: string, endISO: string): string {
  const ms = new Date(endISO).getTime() - new Date(startISO).getTime();
  if (!Number.isFinite(ms) || ms < 0) return '–';
  if (ms < 1000) return `${ms} ms`;
  const s = Math.round(ms / 1000);
  if (s < 60) return `${s}s`;
  return `${Math.floor(s / 60)}m ${s % 60}s`;
}

export function relativeTime(iso: string | null | undefined, now: Date = new Date()): string {
  if (!iso) return '–';
  const diff = new Date(iso).getTime() - now.getTime();
  if (!Number.isFinite(diff)) return '–';
  const abs = Math.abs(diff);
  const units: [number, string][] = [
    [86_400_000, 'd'],
    [3_600_000, 'h'],
    [60_000, 'm']
  ];
  let text = 'now';
  for (const [size, label] of units) {
    if (abs >= size) {
      text = `${Math.floor(abs / size)}${label}`;
      break;
    }
  }
  if (text === 'now') return 'just now';
  return diff < 0 ? `${text} ago` : `in ${text}`;
}

export function formatTokens(n: number): string {
  return n >= 1000 ? `${(n / 1000).toFixed(1)}k` : String(n);
}

/** Groups costs by local calendar day (YYYY-MM-DD), oldest first, filling gaps with 0. */
export function costByDay(
  runs: { started_at: string; cost_usd: number }[],
  days: number,
  now: Date = new Date()
): { day: string; cost: number }[] {
  const key = (d: Date) =>
    `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
  const totals = new Map<string, number>();
  for (let i = days - 1; i >= 0; i--) {
    const d = new Date(now);
    d.setDate(d.getDate() - i);
    totals.set(key(d), 0);
  }
  for (const r of runs) {
    const k = key(new Date(r.started_at));
    if (totals.has(k)) totals.set(k, (totals.get(k) ?? 0) + r.cost_usd);
  }
  return [...totals].map(([day, cost]) => ({ day, cost }));
}

/** Sums cost and run count per job for runs starting on or after sinceDay (YYYY-MM-DD), biggest first. */
export function costByJob(
  runs: { job: string; started_at: string; cost_usd: number }[],
  sinceDay: string
): { job: string; cost: number; runs: number }[] {
  const totals = new Map<string, { cost: number; runs: number }>();
  for (const r of runs) {
    if (r.started_at.slice(0, 10) < sinceDay) continue;
    const t = totals.get(r.job) ?? { cost: 0, runs: 0 };
    totals.set(r.job, { cost: t.cost + r.cost_usd, runs: t.runs + 1 });
  }
  return [...totals].map(([job, t]) => ({ job, ...t })).sort((a, b) => b.cost - a.cost);
}
