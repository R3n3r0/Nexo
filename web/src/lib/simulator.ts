/**
 * Request of the policy simulator form (D-POL-03).
 */
import type { SimulateInput } from '@/services';

export type Tls = 'unset' | 'true' | 'false';

export interface ConditionRow {
  id: number;
  key: string;
  value: string;
}

/** Builds the request of the simulator form. */
export function simulateInput(form: {
  user: string;
  action: string;
  bucket: string;
  object: string;
  sourceIp: string;
  tls: Tls;
  time: Date | null;
  conditions: readonly ConditionRow[];
}): SimulateInput {
  const conditions: Record<string, string[]> = {};
  for (const row of form.conditions) {
    const key = row.key.trim();
    if (key === '') continue;
    conditions[key] = [...(conditions[key] ?? []), row.value];
  }
  return {
    action: form.action.trim(),
    ...(form.user.trim() !== '' ? { user: form.user.trim() } : {}),
    ...(form.bucket.trim() !== '' ? { bucket: form.bucket.trim() } : {}),
    ...(form.object.trim() !== '' ? { object: form.object.trim() } : {}),
    ...(form.sourceIp.trim() !== '' ? { sourceIp: form.sourceIp.trim() } : {}),
    ...(form.tls !== 'unset' ? { secureTransport: form.tls === 'true' } : {}),
    ...(form.time !== null ? { time: form.time.toISOString() } : {}),
    ...(Object.keys(conditions).length > 0 ? { conditions } : {}),
  };
}
