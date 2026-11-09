import { afterEach, describe, expect, it, vi } from 'vitest';
import { downloadJSON } from './download';

describe('downloadJSON', () => {
  afterEach(() => vi.unstubAllGlobals());

  it('clicks an anchor with the filename and a JSON blob', async () => {
    let blob: Blob | undefined;
    const anchor = { href: '', download: '', click: vi.fn(), remove: vi.fn() };
    const revoke = vi.fn();
    vi.stubGlobal('URL', {
      createObjectURL: (b: Blob) => {
        blob = b;
        return 'blob:x';
      },
      revokeObjectURL: revoke
    });
    vi.stubGlobal('document', {
      createElement: () => anchor,
      body: { appendChild: vi.fn() }
    });

    downloadJSON('a-b.json', { id: 'r1', output: 'x' });

    expect(anchor.download).toBe('a-b.json');
    expect(anchor.href).toBe('blob:x');
    expect(anchor.click).toHaveBeenCalledOnce();
    expect(blob?.type).toBe('application/json');
    expect(JSON.parse(await blob!.text())).toEqual({ id: 'r1', output: 'x' });
    expect(revoke).toHaveBeenCalledWith('blob:x');
  });
});
