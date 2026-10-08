import { createHash, createHmac } from 'node:crypto';

/** Access key pair used to sign requests. */
export interface Credentials {
  accessKey: string;
  secretKey: string;
  sessionToken?: string;
}

export interface SignInput {
  method: string;
  url: URL;
  headers?: Record<string, string>;
  body?: Buffer | string;
  region: string;
  /** "s3" for both the S3 API and the Admin API. */
  service?: string;
  now?: Date;
}

const sha256 = (data: Buffer | string): string => createHash('sha256').update(data).digest('hex');

const hmac = (key: Buffer | string, data: string): Buffer =>
  createHmac('sha256', key).update(data).digest();

/** RFC 3986 encoding of AWS: everything but A-Z a-z 0-9 - _ . ~ is escaped. */
export function awsEncode(value: string): string {
  return encodeURIComponent(value).replace(
    /[!'()*]/g,
    (c) => `%${c.charCodeAt(0).toString(16).toUpperCase()}`,
  );
}

/** Encodes a decoded path segment by segment ("/bucket/a b/c" → "/bucket/a%20b/c"). */
export function encodePath(path: string): string {
  return path
    .split('/')
    .map((segment) => awsEncode(segment))
    .join('/');
}

function amzDate(now: Date): string {
  return now
    .toISOString()
    .replace(/[-:]/g, '')
    .replace(/\.\d{3}/, '');
}

/**
 * Signs a request with AWS Signature Version 4 (header form) and returns the
 * headers to send, Authorization included. The URL path must already be
 * encoded with encodePath so that the server canonicalises the same bytes.
 */
export function signRequest(input: SignInput, creds: Credentials): Record<string, string> {
  const now = input.now ?? new Date();
  const stamp = amzDate(now);
  const day = stamp.slice(0, 8);
  const service = input.service ?? 's3';
  const payloadHash = sha256(input.body ?? '');
  const headers: Record<string, string> = {};
  for (const [name, value] of Object.entries(input.headers ?? {})) {
    headers[name.toLowerCase()] = value;
  }
  headers.host = input.url.host;
  headers['x-amz-date'] = stamp;
  headers['x-amz-content-sha256'] = payloadHash;
  if (creds.sessionToken !== undefined) {
    headers['x-amz-security-token'] = creds.sessionToken;
  }
  const names = Object.keys(headers).sort();
  const canonicalHeaders = names
    .map((name) => `${name}:${(headers[name] ?? '').trim().replace(/\s+/g, ' ')}\n`)
    .join('');
  const signedHeaders = names.join(';');
  const query = [...input.url.searchParams.entries()]
    .map(([k, v]) => [awsEncode(k), awsEncode(v)] as const)
    .sort(([ak, av], [bk, bv]) => (ak === bk ? (av < bv ? -1 : 1) : ak < bk ? -1 : 1))
    .map(([k, v]) => `${k}=${v}`)
    .join('&');
  const canonicalRequest = [
    input.method.toUpperCase(),
    input.url.pathname,
    query,
    canonicalHeaders,
    signedHeaders,
    payloadHash,
  ].join('\n');
  const scope = `${day}/${input.region}/${service}/aws4_request`;
  const stringToSign = ['AWS4-HMAC-SHA256', stamp, scope, sha256(canonicalRequest)].join('\n');
  const signingKey = hmac(
    hmac(hmac(hmac(`AWS4${creds.secretKey}`, day), input.region), service),
    'aws4_request',
  );
  const signature = createHmac('sha256', signingKey).update(stringToSign).digest('hex');
  delete headers.host;
  return {
    ...headers,
    authorization: `AWS4-HMAC-SHA256 Credential=${creds.accessKey}/${scope}, SignedHeaders=${signedHeaders}, Signature=${signature}`,
  };
}
