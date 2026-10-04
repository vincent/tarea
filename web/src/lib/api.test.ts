import { describe, expect, it } from 'vitest';
import { ApiError, createApi } from './api';

function fakeFetch(status: number, body: unknown, seen: { url?: string; init?: RequestInit } = {}) {
  return ((url: string, init?: RequestInit) => {
    seen.url = url;
    seen.init = init;
    return Promise.resolve(
      new Response(JSON.stringify(body), {
        status,
        headers: { 'Content-Type': 'application/json' }
      })
    );
  }) as typeof fetch;
}

describe('api client', () => {
  it('encodes path segments and parses JSON', async () => {
    const seen: { url?: string } = {};
    const api = createApi(fakeFetch(200, [{ name: 'a' }], seen));
    const jobs = await api.jobs();
    expect(jobs[0]?.name).toBe('a');

    await api.run('my job', 'id/1').catch(() => undefined);
    expect(seen.url).toBe('/api/jobs/my%20job/runs/id%2F1');
  });

  it('turns error bodies into ApiError', async () => {
    const api = createApi(fakeFetch(409, { error: 'job already running' }));
    await expect(api.runNow('gigs')).rejects.toMatchObject({
      status: 409,
      message: 'job already running'
    });
    await expect(api.runNow('gigs')).rejects.toBeInstanceOf(ApiError);
  });

  it('POSTs run-now', async () => {
    const seen: { init?: RequestInit } = {};
    const api = createApi(fakeFetch(202, { status: 'started' }, seen));
    await api.runNow('gigs');
    expect(seen.init?.method).toBe('POST');
  });

  it('sends the bearer token when one is stored', async () => {
    const seen: { init?: RequestInit } = {};
    const api = createApi(fakeFetch(200, [], seen), () => 's3cret');
    await api.jobs();
    expect(new Headers(seen.init?.headers).get('Authorization')).toBe('Bearer s3cret');

    const anon = createApi(fakeFetch(200, [], seen), () => null);
    await anon.jobs();
    expect(new Headers(seen.init?.headers).has('Authorization')).toBe(false);
  });
});
