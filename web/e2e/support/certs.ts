import { execFileSync } from 'node:child_process';
import { mkdtemp, readFile } from 'node:fs/promises';
import { join } from 'node:path';

/** A PEM certificate and its private key. */
export interface CertificatePair {
  certificatePem: string;
  privateKeyPem: string;
}

/**
 * A self-signed ECDSA certificate of a domain valid for the given days,
 * made with openssl in a new directory under dir (TD-D-04).
 */
export async function selfSigned(
  dir: string,
  domain: string,
  days: number,
): Promise<CertificatePair> {
  const work = await mkdtemp(join(dir, 'cert-'));
  execFileSync(
    'openssl',
    [
      'req',
      '-x509',
      '-newkey',
      'ec',
      '-pkeyopt',
      'ec_paramgen_curve:P-256',
      '-nodes',
      '-days',
      String(days),
      '-subj',
      `/CN=${domain}`,
      '-addext',
      `subjectAltName=DNS:${domain}`,
      '-keyout',
      join(work, 'key.pem'),
      '-out',
      join(work, 'cert.pem'),
    ],
    { stdio: 'ignore' },
  );
  return {
    certificatePem: await readFile(join(work, 'cert.pem'), 'utf8'),
    privateKeyPem: await readFile(join(work, 'key.pem'), 'utf8'),
  };
}
