/**
 * Credential helpers of the IAM pages: random access/secret key generation
 * (crypto.getRandomValues) and the format rules of the IAM service
 * (internal/service/iam/validate.go).
 */
import type { CreatedAccessKey } from '@/services';

export const accessKeyMinLength = 3;
export const accessKeyMaxLength = 20;
export const secretKeyMinLength = 8;
export const secretKeyMaxLength = 40;

const accessKeyAlphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789';
const secretKeyAlphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/';

function randomString(length: number, alphabet: string): string {
  const out: string[] = [];
  // Rejection sampling keeps the distribution uniform for any alphabet size.
  const limit = 256 - (256 % alphabet.length);
  while (out.length < length) {
    const bytes = new Uint8Array(length * 2);
    crypto.getRandomValues(bytes);
    for (const byte of bytes) {
      if (byte < limit && out.length < length) {
        out.push(alphabet[byte % alphabet.length] ?? '');
      }
    }
  }
  return out.join('');
}

/** A random 20-character access key (uppercase letters and digits). */
export function generateAccessKey(): string {
  return randomString(accessKeyMaxLength, accessKeyAlphabet);
}

/** A random 40-character secret key. */
export function generateSecretKey(): string {
  return randomString(secretKeyMaxLength, secretKeyAlphabet);
}

export type AccessKeyProblem = 'required' | 'length' | 'characters';
export type SecretKeyProblem = 'required' | 'length' | 'characters';

/** 3 to 20 characters among letters, digits and -_.@+ (iam.validateAccessKey). */
export function checkAccessKey(value: string): AccessKeyProblem | undefined {
  if (value === '') return 'required';
  if (value.length < accessKeyMinLength || value.length > accessKeyMaxLength) return 'length';
  if (!/^[A-Za-z0-9\-_.@+]+$/.test(value)) return 'characters';
  return undefined;
}

/** 8 to 40 characters without control characters (iam.validateSecretKey). */
export function checkSecretKey(value: string): SecretKeyProblem | undefined {
  if (value === '') return 'required';
  const length = [...value].length;
  if (length < secretKeyMinLength || length > secretKeyMaxLength) return 'length';
  // eslint-disable-next-line no-control-regex -- control characters are what we reject
  if (/[\u0000-\u001f\u007f]/.test(value)) return 'characters';
  return undefined;
}

/** The credentials file offered for download (MinIO-style keys). */
export function credentialsDocument(created: CreatedAccessKey) {
  return {
    accessKey: created.accessKey,
    secretKey: created.secretKey,
    api: 's3v4',
    path: 'auto',
    ...(created.name !== '' ? { name: created.name } : {}),
    parentUser: created.parentUser,
    ...(created.expiration !== undefined ? { expiration: created.expiration } : {}),
  };
}
