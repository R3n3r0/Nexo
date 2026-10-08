/** Binary size units offered by the size inputs (quota). */
export const sizeUnits = ['MiB', 'GiB', 'TiB'] as const;
export type SizeUnit = (typeof sizeUnits)[number];

const factors: Record<SizeUnit, number> = {
  MiB: 1024 ** 2,
  GiB: 1024 ** 3,
  TiB: 1024 ** 4,
};

/** Bytes of a value in a unit, rounded to a whole byte. */
export function toBytes(value: number, unit: SizeUnit): number {
  return Math.round(value * factors[unit]);
}

/** The largest unit expressing bytes as a value >= 1 (MiB for small sizes). */
export function fromBytes(bytes: number): { value: number; unit: SizeUnit } {
  for (const unit of [...sizeUnits].reverse()) {
    if (bytes >= factors[unit] && bytes % (factors[unit] / 1024) === 0) {
      return { value: bytes / factors[unit], unit };
    }
  }
  return { value: bytes / factors.MiB, unit: 'MiB' };
}
