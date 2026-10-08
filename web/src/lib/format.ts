const byteUnits = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB', 'EiB'] as const;

/** Formats a byte count with binary units (1 KiB = 1024 B). */
export function formatBytes(bytes: number, locale?: string): string {
  if (!Number.isFinite(bytes) || bytes < 0) {
    return '—';
  }
  let value = bytes;
  let unit = 0;
  while (value >= 1024 && unit < byteUnits.length - 1) {
    value /= 1024;
    unit += 1;
  }
  const digits = unit === 0 ? 0 : value >= 100 ? 0 : value >= 10 ? 1 : 2;
  const formatted = new Intl.NumberFormat(locale, {
    minimumFractionDigits: 0,
    maximumFractionDigits: digits,
  }).format(value);
  return `${formatted} ${byteUnits[unit] ?? 'B'}`;
}

/** Formats an integer with locale grouping. */
export function formatNumber(value: number, locale?: string): string {
  if (!Number.isFinite(value)) {
    return '—';
  }
  return new Intl.NumberFormat(locale).format(value);
}

/** Compact form for stat tiles: 1,284 / 12.9K / 4.2M. */
export function formatCompact(value: number, locale?: string): string {
  if (!Number.isFinite(value)) {
    return '—';
  }
  if (Math.abs(value) < 10_000) {
    return formatNumber(value, locale);
  }
  return new Intl.NumberFormat(locale, { notation: 'compact', maximumFractionDigits: 1 }).format(
    value,
  );
}

export interface DurationParts {
  days: number;
  hours: number;
  minutes: number;
  seconds: number;
}

/** Splits a number of seconds into days, hours, minutes and seconds. */
export function splitDuration(totalSeconds: number): DurationParts {
  const safe = Math.max(0, Math.floor(Number.isFinite(totalSeconds) ? totalSeconds : 0));
  return {
    days: Math.floor(safe / 86_400),
    hours: Math.floor((safe % 86_400) / 3_600),
    minutes: Math.floor((safe % 3_600) / 60),
    seconds: safe % 60,
  };
}

/**
 * Formats a duration keeping the two most significant non-zero units
 * ("3d 4h", "4h 12m", "12m 5s", "0s"). The unit labels come from the
 * translation function so they follow the UI language.
 */
export function formatDuration(
  totalSeconds: number,
  label: (unit: keyof DurationParts, count: number) => string,
): string {
  const parts = splitDuration(totalSeconds);
  const order: (keyof DurationParts)[] = ['days', 'hours', 'minutes', 'seconds'];
  const chunks: string[] = [];
  for (const unit of order) {
    if (parts[unit] > 0 || (chunks.length > 0 && chunks.length < 2)) {
      chunks.push(label(unit, parts[unit]));
    }
    if (chunks.length === 2) {
      break;
    }
  }
  return chunks.length > 0 ? chunks.join(' ') : label('seconds', 0);
}
