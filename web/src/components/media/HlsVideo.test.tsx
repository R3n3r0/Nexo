import { render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { canStream, playerUrl, streamable } from '@/lib/media';

import { HlsVideo } from './HlsVideo';

describe('HlsVideo', () => {
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('plays the playlist natively when the browser can', () => {
    vi.spyOn(HTMLMediaElement.prototype, 'canPlayType').mockImplementation((type) =>
      type === 'application/vnd.apple.mpegurl' ? 'maybe' : '',
    );
    render(<HlsVideo src="/hls/master.m3u8" fallback="/preview" label="clip.mp4" />);
    const video = screen.getByLabelText<HTMLVideoElement>('clip.mp4');
    expect(video.getAttribute('src')).toBe('/hls/master.m3u8');
  });

  it('falls back to the progressive file without Media Source Extensions', async () => {
    vi.spyOn(HTMLMediaElement.prototype, 'canPlayType').mockReturnValue('');
    render(<HlsVideo src="/hls/master.m3u8" fallback="/preview" label="clip.mp4" />);
    const video = screen.getByLabelText<HTMLVideoElement>('clip.mp4');
    // jsdom has no MediaSource: hls.js is not supported.
    await waitFor(() => {
      expect(video.getAttribute('src')).toBe('/preview');
    });
  });
});

describe('streamable', () => {
  it('streams the MP4 family only', () => {
    expect(streamable('video/mp4')).toBe(true);
    expect(streamable('Video/MP4; codecs="avc1.64001f"')).toBe(true);
    expect(streamable('video/quicktime')).toBe(true);
    expect(streamable('video/webm')).toBe(false);
    expect(streamable(undefined)).toBe(false);
  });

  it('knows the videos of a listing by their extension', () => {
    expect(canStream('clips/a.MP4')).toBe(true);
    expect(canStream('clips/a.mov')).toBe(true);
    expect(canStream('clips/a.webm')).toBe(false);
    expect(canStream('clips/a.bin', 'video/mp4')).toBe(true);
    expect(canStream('clips/a.mp4', 'application/octet-stream')).toBe(false);
  });
});

describe('playerUrl', () => {
  it('puts the playlist in the fragment of the player page', () => {
    expect(
      playerUrl(
        'https://s3.example.com/nexo/media/v1/films/master.m3u8?key=a.mp4&X-Amz-Signature=s',
      ),
    ).toBe(
      'https://s3.example.com/nexo/media/player/#/nexo/media/v1/films/master.m3u8?key=a.mp4&X-Amz-Signature=s',
    );
  });
});
