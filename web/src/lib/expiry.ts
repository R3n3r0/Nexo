/**
 * Validity of share links and presigned URLs: 1 second to 7 days
 * (X-Amz-Expires limit, docs/09 §9.1).
 */

/** Maximum validity in seconds (7 days). */
export const maxExpirySeconds = 7 * 24 * 3600;

/** Presets offered by the share dialog and the upload generator. */
export const expiryPresets = {
  '1h': 3600,
  '1d': 24 * 3600,
  '7d': maxExpirySeconds,
} as const;

export type ExpiryPreset = keyof typeof expiryPresets;

export type ExpiryProblem = 'required' | 'past' | 'tooFar';

/** Seconds from now until a custom expiration date, rounded down. */
export function secondsUntil(date: Date, now: Date = new Date()): number {
  return Math.floor((date.getTime() - now.getTime()) / 1000);
}

/** Checks a custom expiration date against the 7 days limit. */
export function checkExpiryDate(
  date: Date | null,
  now: Date = new Date(),
): ExpiryProblem | undefined {
  if (date === null) return 'required';
  const seconds = secondsUntil(date, now);
  if (seconds < 1) return 'past';
  if (seconds > maxExpirySeconds) return 'tooFar';
  return undefined;
}
