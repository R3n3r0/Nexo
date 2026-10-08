import { isApiError } from '@/services';

/**
 * Human readable message of a failure: the problem detail of an ApiError
 * (the server phrases it for people, naming e.g. the missing permission),
 * its title otherwise, or the message of any other error.
 */
export function errorText(error: unknown, fallback: string): string {
  if (isApiError(error)) {
    if (error.detail !== undefined && error.detail !== '') {
      return error.detail;
    }
    return error.title;
  }
  if (error instanceof Error && error.message !== '') {
    return error.message;
  }
  return fallback;
}
