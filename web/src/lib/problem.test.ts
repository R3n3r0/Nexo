import { afterEach, describe, expect, it } from 'vitest';

import i18n, { changeLanguage, initI18n } from '@/i18n';
import { ApiError } from '@/services';

import { problemMessage } from './problem';

function problem(status: number, code: string, detail: string): ApiError {
  return new ApiError({ title: 'Service Unavailable', status, code, detail });
}

describe('problemMessage', () => {
  afterEach(async () => {
    await changeLanguage('en');
  });

  // A deployment without quorum: the console explains it in the language
  // of the user instead of showing the detail of the server.
  it('translates the errors of a deployment without quorum', async () => {
    await initI18n({ lng: 'en' });
    const lock = problem(503, 'lock_timeout', 'The resource is locked by another operation...');
    expect(problemMessage(i18n.t, lock)).toBe(i18n.t('storageErrors.lock_timeout'));
    expect(problemMessage(i18n.t, problem(503, 'write_quorum', 'x'))).toContain('write quorum');
    expect(problemMessage(i18n.t, problem(503, 'read_quorum', 'x'))).toContain('read quorum');

    await changeLanguage('it');
    for (const code of [
      'lock_timeout',
      'write_quorum',
      'read_quorum',
      'node_offline',
      'disk_not_found',
    ]) {
      const message = problemMessage(i18n.t, problem(503, code, 'detail'));
      expect(message).not.toBe(`storageErrors.${code}`);
      expect(message).not.toBe('detail');
    }
    expect(problemMessage(i18n.t, problem(503, 'write_quorum', 'x'))).toContain(
      'quorum di scrittura',
    );
  });

  it('falls back to the detail of the server for the other codes', async () => {
    await initI18n({ lng: 'en' });
    expect(problemMessage(i18n.t, problem(400, 'invalid_argument', 'logger.level: bad'))).toBe(
      'logger.level: bad',
    );
    expect(problemMessage(i18n.t, new Error('boom'))).toBe('boom');
  });
});
