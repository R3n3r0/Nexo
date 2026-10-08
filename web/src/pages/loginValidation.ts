export const minAccessKeyLength = 3;
export const minSecretKeyLength = 8;

export type LoginFieldError =
  'accessKeyRequired' | 'accessKeyTooShort' | 'secretKeyRequired' | 'secretKeyTooShort';

export interface LoginValidation {
  accessKey?: LoginFieldError;
  secretKey?: LoginFieldError;
}

/** Validates the login form; an empty object means the form can be submitted. */
export function validateLogin(accessKey: string, secretKey: string): LoginValidation {
  const result: LoginValidation = {};
  const key = accessKey.trim();
  if (key === '') {
    result.accessKey = 'accessKeyRequired';
  } else if (key.length < minAccessKeyLength) {
    result.accessKey = 'accessKeyTooShort';
  }
  if (secretKey === '') {
    result.secretKey = 'secretKeyRequired';
  } else if (secretKey.length < minSecretKeyLength) {
    result.secretKey = 'secretKeyTooShort';
  }
  return result;
}

export function isLoginValid(validation: LoginValidation): boolean {
  return validation.accessKey === undefined && validation.secretKey === undefined;
}
