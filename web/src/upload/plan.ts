import type { Upload } from '@/services';

/** Part limits announced by POST /buckets/{bucket}/uploads. */
export type PartLimits = Pick<Upload, 'partSize' | 'minPartSize' | 'maxPartSize' | 'maxParts'>;

/** How a file is split into parts. */
export interface PartPlan {
  partSize: number;
  count: number;
}

/** A byte range of the file, 1-based part number. */
export interface PartRange {
  partNumber: number;
  start: number;
  /** Exclusive end. */
  end: number;
}

const MiB = 1024 * 1024;

/** Raised when a file cannot be split within the limits of the server. */
export class FileTooLargeError extends Error {
  constructor(
    readonly size: number,
    readonly maxSize: number,
  ) {
    super(
      `The file (${String(size)} bytes) exceeds the maximum multipart size (${String(maxSize)} bytes).`,
    );
    this.name = 'FileTooLargeError';
  }
}

/**
 * Chooses the part size of a multipart upload: the recommended size of the
 * server, grown (in whole MiB) when the file would need more than maxParts
 * parts, never below minPartSize nor above maxPartSize.
 */
export function planParts(size: number, limits: PartLimits): PartPlan {
  const maxSize = limits.maxPartSize * limits.maxParts;
  if (size > maxSize) {
    throw new FileTooLargeError(size, maxSize);
  }
  let partSize = Math.max(limits.partSize, limits.minPartSize);
  const needed = Math.ceil(size / limits.maxParts);
  if (needed > partSize) {
    partSize = Math.ceil(needed / MiB) * MiB;
  }
  partSize = Math.min(partSize, limits.maxPartSize);
  const count = Math.max(1, Math.ceil(size / partSize));
  return { partSize, count };
}

/** The byte ranges of the parts of a plan. */
export function partRanges(size: number, plan: PartPlan): PartRange[] {
  const ranges: PartRange[] = [];
  for (let i = 0; i < plan.count; i += 1) {
    const start = i * plan.partSize;
    ranges.push({ partNumber: i + 1, start, end: Math.min(size, start + plan.partSize) });
  }
  return ranges;
}
