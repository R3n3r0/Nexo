import { describe, expect, it, vi } from 'vitest';

import { playbackClaims, startWatermark, watermarkText } from './watermark';

function token(claims: object): string {
  const json = JSON.stringify(claims);
  const b64 = btoa(String.fromCharCode(...new TextEncoder().encode(json)))
    .replace(/\+/g, '-')
    .replace(/\//g, '_')
    .replace(/=+$/, '');
  return `${b64}.c2lnbmF0dXJl`;
}

describe('playbackClaims', () => {
  it('reads the watermark of the token of the playlist', () => {
    const url = new URL('https://s3.example.com/nexo/media/v1/b/master.m3u8');
    url.searchParams.set('key', 'a.mp4');
    url.searchParams.set(
      'pt',
      token({ sub: 'student-7', wm: 'Mario Rossi · mario@example.com', exp: 1 }),
    );
    const claims = playbackClaims(url);
    expect(claims?.sub).toBe('student-7');
    expect(watermarkText(claims)).toBe('Mario Rossi · mario@example.com');
  });

  it('falls back to the subject, and to nothing without token', () => {
    const url = new URL('https://s3.example.com/x?pt=' + token({ sub: 'student-7' }));
    expect(watermarkText(playbackClaims(url))).toBe('student-7');
    expect(playbackClaims(new URL('https://s3.example.com/x'))).toBeUndefined();
    expect(playbackClaims(new URL('https://s3.example.com/x?pt=%%%'))).toBeUndefined();
    expect(watermarkText(undefined)).toBeUndefined();
  });
});

describe('startWatermark', () => {
  it('draws the text and moves it until stopped', () => {
    vi.useFakeTimers();
    const stage = document.createElement('div');
    let n = 0;
    const stop = startWatermark(stage, 'viewer', 1000, () => (n++ % 3) / 3);
    const mark = stage.querySelector<HTMLElement>('.watermark');
    expect(mark?.textContent).toBe('viewer');
    const first = mark?.style.left;
    vi.advanceTimersByTime(1000);
    expect(mark?.style.left).not.toBe(first);
    stop();
    expect(stage.querySelector('.watermark')).toBeNull();
    vi.useRealTimers();
  });
});
