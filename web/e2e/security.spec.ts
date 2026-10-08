import { execFileSync } from 'node:child_process';
import { mkdtemp } from 'node:fs/promises';
import { request as httpRequest } from 'node:http';
import { join } from 'node:path';

import { request as playwrightRequest } from '@playwright/test';

import { uniqueName } from './support/api';
import { repoRoot } from './support/env';
import { rootCredentials, startNexo, stopNexo } from './support/nexo';
import { anonymous, contextAs, expect, loginWithForm, test } from './support/fixtures';

/** Sends a GET with the path exactly as written (no client-side normalisation). */
function rawGet(origin: string, path: string): Promise<{ status: number; body: string }> {
  const { hostname, port } = new URL(origin);
  return new Promise((resolve, reject) => {
    const req = httpRequest({ host: hostname, port, path, method: 'GET' }, (res) => {
      let body = '';
      res.setEncoding('utf8');
      res.on('data', (chunk: string) => {
        body += chunk;
      });
      res.on('end', () => {
        resolve({ status: res.statusCode ?? 0, body });
      });
    });
    req.on('error', reject);
    req.end();
  });
}

test.describe('console security', () => {
  test('TD-S-01 a mutating request without the CSRF token is refused', async ({ page, api }) => {
    await page.goto('/');
    const csrf = (await page.context().cookies()).find((c) => c.name === 'nexo_csrf')?.value;
    expect(csrf).toBeTruthy();
    const bucket = uniqueName('e2e-csrf');
    try {
      // The session cookie alone is not enough.
      let res = await page.request.post('/api/v1/buckets', { data: { name: bucket } });
      expect(res.status()).toBe(403);
      expect((await res.json()) as { code: string }).toMatchObject({ code: 'invalid_csrf_token' });
      res = await page.request.post('/api/v1/buckets', {
        data: { name: bucket },
        headers: { 'X-CSRF-Token': 'forged-token' },
      });
      expect(res.status()).toBe(403);
      res = await page.request.delete(`/api/v1/buckets/${bucket}`);
      expect(res.status()).toBe(403);
      expect((await api.admin('GET', `/buckets/${bucket}`)).status).toBe(404);

      // With the token of the session the same request succeeds.
      res = await page.request.post('/api/v1/buckets', {
        data: { name: bucket },
        headers: { 'X-CSRF-Token': csrf ?? '' },
      });
      expect(res.status()).toBe(201);
      // Reads need no token.
      expect((await page.request.get('/api/v1/buckets')).status()).toBe(200);
    } finally {
      await api.deleteBucket(bucket);
    }
  });

  test.describe('cookies', () => {
    test.use({ storageState: anonymous });

    test('TD-S-02 session cookies are HttpOnly, SameSite=Strict, and Secure over TLS', async ({
      page,
      context,
      nexo,
    }) => {
      await page.goto('/login');
      await loginWithForm(page, rootCredentials);
      const cookies = await context.cookies();
      const session = cookies.find((c) => c.name === 'nexo_session');
      const csrf = cookies.find((c) => c.name === 'nexo_csrf');
      expect(session).toMatchObject({ httpOnly: true, sameSite: 'Strict', path: '/' });
      // The CSRF cookie is read by the console (double submit).
      expect(csrf).toMatchObject({ httpOnly: false, sameSite: 'Strict' });
      // Not readable from the page.
      expect(await page.evaluate(() => document.cookie)).not.toContain('nexo_session');

      // Over TLS the cookies are Secure too.
      const workDir = await mkdtemp(join(nexo.workDir, 'tls-'));
      execFileSync(
        'openssl',
        [
          'req',
          '-x509',
          '-newkey',
          'rsa:2048',
          '-nodes',
          '-days',
          '1',
          '-subj',
          '/CN=127.0.0.1',
          '-addext',
          'subjectAltName=IP:127.0.0.1',
          '-keyout',
          join(workDir, 'private.key'),
          '-out',
          join(workDir, 'public.crt'),
        ],
        { stdio: 'ignore' },
      );
      const tls = await startNexo({
        binary: process.env.NEXO_E2E_BINARY || join(repoRoot, 'bin', 'nexo'),
        workDir,
        certsDir: workDir,
      });
      try {
        const ctx = await playwrightRequest.newContext({
          baseURL: tls.consoleURL,
          ignoreHTTPSErrors: true,
        });
        const res = await ctx.post('/api/v1/auth/login', { data: rootCredentials });
        expect(res.status()).toBe(200);
        const setCookies = res
          .headersArray()
          .filter((h) => h.name.toLowerCase() === 'set-cookie')
          .map((h) => h.value);
        const sessionCookie = setCookies.find((c) => c.startsWith('nexo_session='));
        expect(sessionCookie).toMatch(/;\s*HttpOnly/i);
        expect(sessionCookie).toMatch(/;\s*Secure/i);
        expect(sessionCookie).toMatch(/;\s*SameSite=Strict/i);
        expect(setCookies.find((c) => c.startsWith('nexo_csrf='))).toMatch(/;\s*Secure/i);
        await ctx.dispose();
      } finally {
        await stopNexo(tls.pid);
      }
    });
  });

  test('TD-S-03 an object key with markup is shown as text, never executed', async ({
    page,
    api,
  }) => {
    const bucket = uniqueName('e2e-xss');
    await api.createBucket(bucket);
    const imgKey = '<img src=x onerror=window.__xss=1>.txt';
    await api.putObject(bucket, imgKey, 'x', 'text/plain');
    await api.putObject(bucket, '<script>window.__xss=2</script>.txt', 'y', 'text/plain');
    try {
      const dialogs: string[] = [];
      page.on('dialog', (dialog) => {
        dialogs.push(dialog.message());
        void dialog.dismiss();
      });
      await page.goto(`/buckets/${bucket}/browse/`);
      const name = page.getByRole('button', { name: imgKey, exact: true });
      await expect(name).toBeVisible();
      // The "</script>" made a folder: shown literally too.
      await expect(
        page.getByRole('link', { name: '<script>window.__xss=2</', exact: true }),
      ).toBeVisible();
      await name.click();
      await expect(page.getByRole('dialog', { name: imgKey })).toBeVisible();
      await page.getByRole('button', { name: 'Close' }).click();
      await page.getByRole('link', { name: '<script>window.__xss=2</', exact: true }).click();
      await expect(page.getByRole('button', { name: 'script>.txt', exact: true })).toBeVisible();

      expect(
        await page.evaluate(() => (window as unknown as { __xss?: number }).__xss),
      ).toBeUndefined();
      expect(await page.locator('img[src="x"]').count()).toBe(0);
      expect(dialogs).toEqual([]);
    } finally {
      await api.deleteBucket(bucket);
    }
  });

  test('TD-S-04 HTML and SVG objects are never rendered in the console origin', async ({
    page,
    api,
  }) => {
    const bucket = uniqueName('e2e-active');
    await api.createBucket(bucket);
    await api.putObject(
      bucket,
      'evil.html',
      '<html><body><script>window.top.__pwned = "html"; alert("html")</script>page</body></html>',
      'text/html',
    );
    await api.putObject(
      bucket,
      'evil.svg',
      '<svg xmlns="http://www.w3.org/2000/svg" onload="window.top.__pwned=\'svg\';alert(1)"><circle r="5"/></svg>',
      'image/svg+xml',
    );
    try {
      const dialogs: string[] = [];
      page.on('dialog', (dialog) => {
        dialogs.push(dialog.message());
        void dialog.dismiss();
      });
      await page.goto(`/buckets/${bucket}/browse/`);
      for (const [name, type] of [
        ['evil.html', 'text/html'],
        ['evil.svg', 'image/svg+xml'],
      ] as const) {
        await page.getByRole('button', { name, exact: true }).click();
        const drawer = page.getByRole('dialog', { name });
        await drawer.getByRole('tab', { name: 'Preview' }).click();
        await expect(drawer.getByText(`No preview for ${type}`)).toBeVisible();
        expect(await drawer.locator('iframe, object, embed').count()).toBe(0);

        // The endpoints serve them as sandboxed attachments.
        for (const endpoint of ['preview', 'download']) {
          const res = await page.request.get(
            `/api/v1/buckets/${bucket}/objects/${endpoint}?key=${encodeURIComponent(name)}`,
          );
          expect(res.status(), `${endpoint} ${name}`).toBe(200);
          expect(res.headers()['content-disposition'], `${endpoint} ${name}`).toMatch(
            /^attachment/,
          );
          expect(res.headers()['x-content-type-options']).toBe('nosniff');
          expect(res.headers()['content-security-policy']).toContain('sandbox');
        }
        // Opening the link in the browser downloads it instead of rendering.
        const download = page.waitForEvent('download');
        await page.evaluate(
          (url) => {
            window.open(url, '_self');
          },
          `/api/v1/buckets/${bucket}/objects/preview?key=${encodeURIComponent(name)}`,
        );
        expect((await download).suggestedFilename()).toBe(name);
        await page.goto(`/buckets/${bucket}/browse/`);
      }
      expect(
        await page.evaluate(() => (window as unknown as { __pwned?: string }).__pwned),
      ).toBeUndefined();
      expect(dialogs).toEqual([]);
    } finally {
      await api.deleteBucket(bucket);
    }
  });

  test('TD-S-05 index.html carries the security headers and its CSP is enforced', async ({
    page,
    request,
  }) => {
    for (const path of ['/', '/buckets/x/browse/a/']) {
      const res = await request.get(path);
      const headers = res.headers();
      expect(headers['content-security-policy'], path).toContain("default-src 'self'");
      expect(headers['content-security-policy'], path).toContain("script-src 'self'");
      expect(headers['content-security-policy'], path).toContain("frame-ancestors 'none'");
      expect(headers['x-frame-options'], path).toBe('DENY');
      expect(headers['referrer-policy'], path).toBe('strict-origin-when-cross-origin');
      expect(headers['x-content-type-options'], path).toBe('nosniff');
    }
    // An injected inline script is blocked by the policy.
    await page.goto('/');
    const violation = await page.evaluate(
      () =>
        new Promise<string>((resolve) => {
          document.addEventListener('securitypolicyviolation', (event) => {
            resolve(event.violatedDirective);
          });
          const script = document.createElement('script');
          script.textContent = 'window.__inline = true';
          document.body.appendChild(script);
        }),
    );
    expect(violation).toMatch(/^script-src/);
    expect(
      await page.evaluate(() => (window as unknown as { __inline?: boolean }).__inline),
    ).toBeUndefined();
  });

  test('TD-S-06 a user without admin permissions gets 403 from /api/v1/users', async ({
    browser,
    nexo,
    api,
  }) => {
    const context = await contextAs(browser, nexo, nexo.users.member);
    try {
      const res = await context.request.get('/api/v1/users');
      expect(res.status()).toBe(403);
      expect((await res.json()) as { code: string }).toMatchObject({ code: 'access_denied' });
      const csrf = (await context.cookies()).find((c) => c.name === 'nexo_csrf')?.value ?? '';
      const create = await context.request.post('/api/v1/users', {
        data: { accessKey: 'sneaky-user', secretKey: 'sneaky-secret-1' },
        headers: { 'X-CSRF-Token': csrf },
      });
      expect(create.status()).toBe(403);
      // The same through SigV4.
      expect((await api.as(nexo.users.member).admin('GET', '/users')).status).toBe(403);
      expect((await api.admin('GET', '/users/sneaky-user')).status).toBe(404);
    } finally {
      await context.close();
    }
  });

  test('TD-S-07 path traversal on the static assets answers 404', async ({ nexo }) => {
    for (const path of [
      '/../../etc/passwd',
      '/assets/../../../etc/passwd',
      '/%2e%2e/%2e%2e/etc/passwd',
      '/assets/%2e%2e%2f%2e%2e%2f%2e%2e%2fetc%2fpasswd',
      '/..%2f..%2f..%2fetc%2fpasswd',
      '/assets/..%5c..%5c..%5cetc%5cpasswd',
      '/assets/%252e%252e/%252e%252e/etc/passwd',
    ]) {
      const res = await rawGet(nexo.consoleURL, path);
      expect(res.status, path).toBe(404);
      expect(res.body, path).not.toContain('root:');
      expect(res.body, path).not.toContain('<div id="root">');
    }
  });
});
