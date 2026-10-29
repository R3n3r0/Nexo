import type { Session, SessionOptions } from './index.js';

export function openSession(playlist: string | URL, options?: SessionOptions): Promise<Session>;
export function sessionLoader(Hls: unknown, session: Session): unknown;
export function sessionURL(playlist: string | URL): URL;
export function isSegmentURL(url: string): boolean;
export function keyAAD(rev: string, rendition: string, period: number): string;
