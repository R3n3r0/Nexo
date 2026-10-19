import { describe, expect, it } from 'vitest';

import type { ApiError } from '../errors';
import { createMockServices } from './index';

const admin = () => createMockServices({ initialAccessKey: 'nexoadmin' });

describe('mock media service', () => {
  it('stores the rules normalized like the server', async () => {
    const { media } = admin();
    expect(await media.get('media')).toEqual({ rules: [] });
    const stored = await media.set('media', {
      rules: [
        {
          id: 'videos',
          prefix: 'videos/',
          suffixes: ['.MP4', '.mp4'],
          renditions: ['240p', '1080p'],
          thumbnails: { interval: '90s' },
        },
      ],
    });
    expect(stored.rules).toEqual([
      {
        id: 'videos',
        prefix: 'videos/',
        suffixes: ['.mp4'],
        renditions: ['1080p', '240p'],
        thumbnails: { interval: '1m30s' },
      },
    ]);
    expect(await media.get('media')).toEqual(stored);
    await media.remove('media');
    expect(await media.get('media')).toEqual({ rules: [] });
  });

  it('reports every invalid rule with its index', async () => {
    const { media } = admin();
    const error = (await media
      .set('media', {
        rules: [
          { id: 'ok', poster: true },
          { id: 'ok', poster: true },
          { id: 'bad id', poster: true },
          { id: 'empty' },
          { id: 'sidecar', prefix: '.nexo-media/x', poster: true },
        ],
      })
      .catch((e: unknown) => e)) as ApiError;
    expect(error.code).toBe('invalid_configuration');
    expect(error.ruleErrors.map((p) => p.rule)).toEqual([2, 3, 4, 5]);
  });

  it('reprocesses a selected video and reports its status', async () => {
    const { media } = admin();
    expect(await media.status('media', 'videos/intro.mp4')).toEqual({
      state: 'none',
      current: false,
    });
    const refused = (await media
      .reprocess('media', { key: 'videos/intro.mp4' })
      .catch((e: unknown) => e)) as ApiError;
    expect(refused.status).toBe(400);

    await media.set('media', {
      rules: [{ id: 'v', renditions: ['2160p', '720p'], poster: true }],
    });
    await media.reprocess('media', { key: 'videos/intro.mp4' });
    const status = await media.status('media', 'videos/intro.mp4');
    expect(status).toMatchObject({ state: 'done', current: true, rule: 'v', poster: true });
    // The 2160p rendition is taller than the 1080p source: skipped.
    expect(status.renditions?.map((r) => r.name)).toEqual(['720p']);
  });
});
