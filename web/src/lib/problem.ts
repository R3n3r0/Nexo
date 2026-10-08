import type { TFunction } from 'i18next';

import { isApiError } from '@/services';

/** Problem codes with a dedicated, translated message under storageErrors.*. */
const knownCodes = [
  'access_denied',
  'bucket_already_exists',
  'bucket_not_empty',
  'bucket_not_found',
  'conflict',
  'entity_too_large',
  'invalid_bucket_name',
  'invalid_object_name',
  'malformed_policy',
  'network_error',
  'object_locked',
  'object_not_found',
  'quota_exceeded',
] as const;

type KnownCode = (typeof knownCodes)[number];

function isKnown(code: string): code is KnownCode {
  return (knownCodes as readonly string[]).includes(code);
}

/**
 * A human message for a failed call: the translation of a well-known
 * problem code, otherwise the detail sent by the server.
 */
export function problemMessage(t: TFunction, error: unknown): string {
  if (isApiError(error)) {
    if (isKnown(error.code)) {
      return t(`storageErrors.${error.code}`);
    }
    return error.detail ?? error.title;
  }
  return error instanceof Error ? error.message : t('common.unknownError');
}
