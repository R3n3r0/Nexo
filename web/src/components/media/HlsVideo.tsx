import { useEffect, useRef } from 'react';

/** The HLS MIME type browsers play natively (Safari, iOS, some Android). */
const hlsType = 'application/vnd.apple.mpegurl';

export interface HlsVideoProps {
  /** URL of the HLS master playlist. */
  src: string;
  /** URL of the progressive file, played when HLS is unavailable or fails. */
  fallback?: string | undefined;
  label: string;
  maxHeight?: number;
}

/**
 * A video element playing an HLS presentation: natively where the browser
 * can, with hls.js (loaded on demand, outside the initial bundle) where it
 * has Media Source Extensions, and from the progressive file otherwise or
 * when the stream fails (a video the server cannot package).
 */
export function HlsVideo({ src, fallback, label, maxHeight = 480 }: HlsVideoProps) {
  const ref = useRef<HTMLVideoElement>(null);

  useEffect(() => {
    const video = ref.current;
    if (!video) return undefined;
    const progressive = () => {
      if (fallback !== undefined) video.src = fallback;
    };
    if (video.canPlayType(hlsType) !== '') {
      video.src = src;
      video.addEventListener('error', progressive, { once: true });
      return () => {
        video.removeEventListener('error', progressive);
      };
    }
    let cancelled = false;
    let destroy: (() => void) | undefined;
    import('hls.js')
      .then(({ default: Hls }) => {
        if (cancelled) return;
        if (!Hls.isSupported()) {
          progressive();
          return;
        }
        // Same origin: the session cookie goes with every request.
        const hls = new Hls();
        destroy = () => hls.destroy();
        hls.on(Hls.Events.ERROR, (_event, data) => {
          if (!data.fatal) return;
          hls.destroy();
          destroy = undefined;
          progressive();
        });
        hls.loadSource(src);
        hls.attachMedia(video);
      })
      .catch(progressive);
    return () => {
      cancelled = true;
      destroy?.();
    };
  }, [src, fallback]);

  return (
    <video ref={ref} controls playsInline style={{ width: '100%', maxHeight }} aria-label={label}>
      <track kind="captions" />
    </video>
  );
}
