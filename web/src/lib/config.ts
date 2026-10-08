/**
 * Client-side rules of the dynamic configuration, mirroring checkValue in
 * internal/service/admin/configmgr.go. The configuration form uses them
 * for immediate feedback; the server validates again on save.
 */
import type { ConfigKey } from '@/services';

/** Placeholder the server returns instead of a stored sensitive value. */
export const sensitiveMask = '********';

/** Problems of a configuration value, as i18n codes. */
export type ConfigValueProblem = 'bool' | 'int' | 'duration' | 'url' | 'enum' | 'list';

const durationPattern = /^(\d+(\.\d+)?(ns|us|µs|ms|s|m|h))+$/;

/**
 * Checks a value against its key; the empty string resets the key to its
 * default and is always valid. Returns the problem, if any.
 */
export function checkConfigValue(key: ConfigKey, value: string): ConfigValueProblem | undefined {
  if (value === '' || (key.sensitive && value === sensitiveMask)) {
    return undefined;
  }
  switch (key.type) {
    case 'bool':
      return ['on', 'off', 'true', 'false'].includes(value.toLowerCase()) ? undefined : 'bool';
    case 'int':
      return /^\d+$/.test(value) ? undefined : 'int';
    case 'duration':
      return value === '0' || durationPattern.test(value) ? undefined : 'duration';
    case 'url':
      try {
        const url = new URL(value);
        return (url.protocol === 'http:' || url.protocol === 'https:') && url.host !== ''
          ? undefined
          : 'url';
      } catch {
        return 'url';
      }
    case 'enum':
      return (key.values ?? []).includes(value) ? undefined : 'enum';
    case 'list':
      return value.split(',').every((item) => item.trim() !== '') ? undefined : 'list';
    default:
      return undefined;
  }
}

/** True for the "on" spellings of a boolean setting. */
export function isConfigTrue(value: string | undefined): boolean {
  return value !== undefined && ['on', 'true'].includes(value.toLowerCase());
}
