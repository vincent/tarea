import { describe, expect, it } from 'vitest';
import {
  costByDay,
  costByJob,
  formatCost,
  formatDuration,
  formatTokens,
  relativeTime
} from './format';

describe('formatCost', () => {
  it('uses more precision for tiny amounts', () => {
    expect(formatCost(0)).toBe('$0');
    expect(formatCost(0.0042)).toBe('$0.0042');
    expect(formatCost(1.234)).toBe('$1.23');
  });
});

describe('formatDuration', () => {
  it('formats ms, seconds and minutes', () => {
    const t = '2026-10-04T08:00:00Z';
    expect(formatDuration(t, '2026-10-04T08:00:00.400Z')).toBe('400 ms');
    expect(formatDuration(t, '2026-10-04T08:00:12Z')).toBe('12s');
    expect(formatDuration(t, '2026-10-04T08:02:05Z')).toBe('2m 5s');
    expect(formatDuration(t, 'garbage')).toBe('–');
  });
});

describe('relativeTime', () => {
  const now = new Date('2026-10-04T12:00:00Z');
  it('handles past, future and missing values', () => {
    expect(relativeTime('2026-10-04T10:00:00Z', now)).toBe('2h ago');
    expect(relativeTime('2026-10-05T12:00:00Z', now)).toBe('in 1d');
    expect(relativeTime('2026-10-04T12:00:10Z', now)).toBe('just now');
    expect(relativeTime(null, now)).toBe('–');
  });
});

describe('formatTokens', () => {
  it('abbreviates thousands', () => {
    expect(formatTokens(950)).toBe('950');
    expect(formatTokens(12_345)).toBe('12.3k');
  });
});

describe('costByDay', () => {
  it('fills gaps and ignores runs outside the window', () => {
    const now = new Date(2026, 9, 4, 12, 0, 0);
    const runs = [
      { started_at: new Date(2026, 9, 4, 8).toISOString(), cost_usd: 0.02 },
      { started_at: new Date(2026, 9, 4, 9).toISOString(), cost_usd: 0.03 },
      { started_at: new Date(2026, 9, 2, 9).toISOString(), cost_usd: 0.5 },
      { started_at: new Date(2026, 8, 1, 9).toISOString(), cost_usd: 99 }
    ];
    const out = costByDay(runs, 3, now);
    expect(out.map((d) => d.day)).toEqual(['2026-10-02', '2026-10-03', '2026-10-04']);
    expect(out.map((d) => Number(d.cost.toFixed(2)))).toEqual([0.5, 0, 0.05]);
  });
});

describe('costByJob', () => {
  it('aggregates per job, newest window only, biggest first', () => {
    const runs = [
      { job: 'a', started_at: '2026-10-04T08:00:00Z', cost_usd: 0.1 },
      { job: 'b', started_at: '2026-10-03T08:00:00Z', cost_usd: 0.3 },
      { job: 'a', started_at: '2026-10-02T08:00:00Z', cost_usd: 0.1 },
      { job: 'a', started_at: '2026-09-01T08:00:00Z', cost_usd: 50 }
    ];
    const out = costByJob(runs, '2026-10-01');
    expect(out.map((r) => r.job)).toEqual(['b', 'a']);
    expect(out[1]).toMatchObject({ runs: 2 });
    expect(out[1]?.cost).toBeCloseTo(0.2);
  });
});
