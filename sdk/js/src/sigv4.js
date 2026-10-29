// AWS Signature Version 4 (service s3), as the Nexo Admin API expects it:
// the path is signed as sent, the payload hash is in x-amz-content-sha256.
import { createHash, createHmac } from 'node:crypto';

const sha256 = (/** @type {string | Uint8Array} */ data) => createHash('sha256').update(data).digest('hex');
const hmac = (/** @type {string | Buffer} */ key, /** @type {string} */ data) => createHmac('sha256', key).update(data).digest();

/** RFC 3986 encoding of a query component. */
export function encodeRfc3986(/** @type {string} */ s) {
  return encodeURIComponent(s).replace(/[!'()*]/g, (c) => '%' + c.charCodeAt(0).toString(16).toUpperCase());
}

/**
 * Signs a request: returns the headers to send (host and the given ones
 * included).
 * @param {{method: string, url: URL, headers?: Record<string,string>, body?: string | Uint8Array,
 *   accessKey: string, secretKey: string, sessionToken?: string, region?: string, service?: string, date?: Date}} req
 * @returns {Record<string, string>}
 */
export function signRequest(req) {
  const region = req.region || 'us-east-1';
  const service = req.service || 's3';
  const date = req.date || new Date();
  const amzDate = date.toISOString().replace(/[:-]|\.\d{3}/g, '');
  const day = amzDate.slice(0, 8);
  const body = req.body ?? '';
  /** @type {Record<string, string>} */
  const headers = {};
  for (const [k, v] of Object.entries(req.headers ?? {})) headers[k.toLowerCase()] = String(v).trim();
  headers.host = req.url.host;
  headers['x-amz-date'] = amzDate;
  headers['x-amz-content-sha256'] ??= sha256(body);
  if (req.sessionToken) headers['x-amz-security-token'] = req.sessionToken;

  const names = Object.keys(headers).sort();
  const canonicalHeaders = names.map((n) => `${n}:${headers[n].replace(/\s+/g, ' ')}\n`).join('');
  const signedHeaders = names.join(';');
  const query = [...req.url.searchParams]
    .map(([k, v]) => [encodeRfc3986(k), encodeRfc3986(v)])
    .sort((a, b) => (a[0] === b[0] ? (a[1] < b[1] ? -1 : 1) : a[0] < b[0] ? -1 : 1))
    .map(([k, v]) => `${k}=${v}`)
    .join('&');
  const canonical = [req.method.toUpperCase(), req.url.pathname || '/', query, canonicalHeaders, signedHeaders, headers['x-amz-content-sha256']].join('\n');
  const scope = `${day}/${region}/${service}/aws4_request`;
  const toSign = ['AWS4-HMAC-SHA256', amzDate, scope, sha256(canonical)].join('\n');
  const key = hmac(hmac(hmac(hmac('AWS4' + req.secretKey, day), region), service), 'aws4_request');
  const signature = createHmac('sha256', key).update(toSign).digest('hex');
  headers.authorization = `AWS4-HMAC-SHA256 Credential=${req.accessKey}/${scope}, SignedHeaders=${signedHeaders}, Signature=${signature}`;
  delete headers.host;
  return headers;
}
