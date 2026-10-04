import type { Health, JobDetail, JobView, MemoryView, RunRecord, RunSummary } from './types';

export class ApiError extends Error {
  constructor(
    readonly status: number,
    message: string
  ) {
    super(message);
    this.name = 'ApiError';
  }
}

type Fetch = typeof fetch;

/** Small typed client; pass a custom fetch in tests. */
export function createApi(fetchFn: Fetch = (...args) => fetch(...args)) {
  async function request<T>(path: string, init?: RequestInit): Promise<T> {
    const res = await fetchFn(path, init);
    const body: unknown = await res.json().catch(() => null);
    if (!res.ok) {
      const msg =
        body && typeof body === 'object' && 'error' in body ? String(body.error) : res.statusText;
      throw new ApiError(res.status, msg);
    }
    return body as T;
  }

  const enc = encodeURIComponent;
  return {
    health: () => request<Health>('/api/health'),
    jobs: () => request<JobView[]>('/api/jobs'),
    job: (name: string) => request<JobDetail>(`/api/jobs/${enc(name)}`),
    jobRuns: (name: string, limit = 50) =>
      request<RunSummary[]>(`/api/jobs/${enc(name)}/runs?limit=${limit}`),
    run: (name: string, id: string) => request<RunRecord>(`/api/jobs/${enc(name)}/runs/${enc(id)}`),
    memory: (name: string) => request<MemoryView>(`/api/jobs/${enc(name)}/memory`),
    allRuns: (limit = 500) => request<RunSummary[]>(`/api/runs?limit=${limit}`),
    runNow: (name: string) =>
      request<{ status: string }>(`/api/jobs/${enc(name)}/run`, { method: 'POST' })
  };
}

export const api = createApi();
