import { describe, expect, it } from 'vitest';

import { createMockServices, sampleClusterInfo } from './index';

describe('mock HealService', () => {
  it('refuses on a single drive (501)', async () => {
    const { heal } = createMockServices();
    await expect(heal.list()).rejects.toMatchObject({ status: 501, code: 'not_implemented' });
    await expect(heal.start({})).rejects.toMatchObject({ status: 501 });
  });

  it('runs a job over the drives of the cluster', async () => {
    const { heal } = createMockServices({ info: sampleClusterInfo() });
    const job = await heal.start({ bucket: 'photos', recursive: true });
    expect(job).toMatchObject({ id: 'heal-0001', state: 'running', finished: false });
    expect(job.drives).toHaveLength(8);
    expect(job.drives.filter((d) => d.state === 'offline')).toHaveLength(2);
    // An identical running job is returned instead of a new one.
    await expect(heal.start({ bucket: 'photos', recursive: true })).resolves.toMatchObject({
      id: 'heal-0001',
    });

    await expect(heal.get(job.id)).resolves.toMatchObject({ state: 'running' });
    const done = await heal.get(job.id);
    expect(done).toMatchObject({ state: 'done', finished: true, healed: 1, scanned: 2 });
    const item = done.items[0];
    expect(item?.object).toBe('report.pdf');
    expect(item?.drives.filter((d) => d.before === 'missing' && d.after === 'ok')).toHaveLength(1);
    expect(item?.drives.filter((d) => d.before === 'offline')).toHaveLength(2);
    await expect(heal.list()).resolves.toHaveLength(1);
    await expect(heal.get('nope')).rejects.toMatchObject({ status: 404 });
  });

  it('only reports with a dry run and validates the request', async () => {
    const { heal } = createMockServices({ info: sampleClusterInfo(), healPolls: 0 });
    const job = await heal.start({ bucket: 'photos', recursive: true, dryRun: true });
    expect(job.finished).toBe(true);
    expect(job.items[0]?.drives.some((d) => d.before === 'missing' && d.after === 'missing')).toBe(
      true,
    );
    await expect(heal.start({ prefix: 'x/' })).rejects.toMatchObject({ status: 400 });
  });
});
