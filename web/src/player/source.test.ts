import { describe, expect, it } from 'vitest';

import { masterUrl } from './source';

const origin = 'https://s3.example.com';

describe('masterUrl', () => {
  it('resolves the playlist of the fragment on the origin of the page', () => {
    const url = masterUrl({
      origin,
      hash: '#/nexo/media/v1/films/master.m3u8?key=a%20b.mp4&X-Amz-Signature=s',
    });
    expect(url?.toString()).toBe(
      'https://s3.example.com/nexo/media/v1/films/master.m3u8?key=a%20b.mp4&X-Amz-Signature=s',
    );
  });

  it('refuses other origins and other paths', () => {
    for (const hash of [
      '',
      '#',
      '#https://evil.example/nexo/media/v1/films/master.m3u8',
      '#//evil.example/nexo/media/v1/films/master.m3u8',
      '#/films/a.mp4',
      '#/nexo/media/v1/films/media.m3u8?key=a',
      '#/nexo/media/v1/films/../../../x/master.m3u8',
      '#%2F%2Fevil.example%2Fnexo%2Fmedia%2Fv1%2Ff%2Fmaster.m3u8',
    ]) {
      expect(masterUrl({ origin, hash })).toBeUndefined();
    }
  });
});
