import { describe, expect, it } from 'vitest';

import { isLoginValid, validateLogin } from './loginValidation';

describe('validateLogin', () => {
  it('requires both fields', () => {
    const v = validateLogin('', '');
    expect(v).toEqual({ accessKey: 'accessKeyRequired', secretKey: 'secretKeyRequired' });
    expect(isLoginValid(v)).toBe(false);
  });

  it('enforces minimum lengths and ignores surrounding spaces in the access key', () => {
    expect(validateLogin(' ab ', 'short')).toEqual({
      accessKey: 'accessKeyTooShort',
      secretKey: 'secretKeyTooShort',
    });
    expect(validateLogin('   ', 'longenough')).toEqual({ accessKey: 'accessKeyRequired' });
  });

  it('accepts valid input', () => {
    const v = validateLogin('admin', 'supersecret1');
    expect(v).toEqual({});
    expect(isLoginValid(v)).toBe(true);
  });
});
