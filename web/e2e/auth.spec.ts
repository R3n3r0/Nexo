import { uniqueKey, uniqueName } from './support/api';
import { modeLabels } from './support/env';
import { rootCredentials } from './support/nexo';
import { anonymous, expect, loginWithForm, test } from './support/fixtures';

test.describe('authentication', () => {
  test.use({ storageState: anonymous });

  test('TD-E-01 root signs in, sees the overview and signs out', async ({
    page,
    context,
    nexo,
  }) => {
    await page.goto('/');
    await expect(page).toHaveURL(/\/login$/);
    await loginWithForm(page, rootCredentials);
    await expect(page).toHaveURL(/\/$/);
    await expect(page.getByRole('heading', { name: 'Overview', level: 2 })).toBeVisible();
    await expect(page.getByText(modeLabels[nexo.mode]).first()).toBeVisible();

    const session = (await context.cookies()).find((c) => c.name === 'nexo_session');
    expect(session?.value).toBeTruthy();

    await page.getByRole('button', { name: `Signed in as ${rootCredentials.accessKey}` }).click();
    await page.getByRole('menuitem', { name: 'Sign out' }).click();
    await expect(page).toHaveURL(/\/login$/);
    // The server clears the session cookie (and revokes the token).
    expect((await context.cookies()).filter((c) => c.name === 'nexo_session')).toEqual([]);
    const reuse = await context.request.get('/api/v1/auth/session', {
      headers: { cookie: `nexo_session=${session?.value ?? ''}` },
    });
    expect(reuse.status()).toBe(401);

    // A protected page sends back to the login.
    await page.goto('/buckets');
    await expect(page).toHaveURL(/\/login$/);
    await expect(page.getByRole('heading', { name: 'Sign in to Nexo' })).toBeVisible();
  });

  test('TD-E-02 repeated wrong credentials are reported, then throttled', async ({ page, api }) => {
    const user = { accessKey: uniqueKey('throttled'), secretKey: 'right-secret-123' };
    await api.call('POST', '/users', { json: { ...user, policyNames: ['readonly'] } });

    await page.goto('/login');
    const accessKey = page.getByRole('textbox', { name: 'Access key' });
    const secretKey = page.getByRole('textbox', { name: 'Secret key' });
    const submit = page.getByRole('button', { name: 'Sign in' });
    const alert = page.getByRole('alert');
    await accessKey.fill(user.accessKey);
    for (let attempt = 1; attempt <= 5; attempt++) {
      await secretKey.fill(`wrong-secret-${attempt}`);
      await submit.click();
      await expect(alert).toHaveText('Invalid access key or secret key.');
      await expect(page).toHaveURL(/\/login$/);
    }
    // The sixth attempt is refused even with the right secret.
    await secretKey.fill(user.secretKey);
    const answer = page.waitForResponse('**/api/v1/auth/login');
    await submit.click();
    const res = await answer;
    expect(res.status()).toBe(429);
    expect(Number(res.headers()['retry-after'])).toBeGreaterThan(0);
    await expect(alert).toHaveText(/Too many failed attempts/);
    await expect(page).toHaveURL(/\/login$/);

    // Another account is not affected by the lockout.
    await accessKey.fill(rootCredentials.accessKey);
    await secretKey.fill(rootCredentials.secretKey);
    await submit.click();
    await expect(
      page.getByRole('button', { name: `Signed in as ${rootCredentials.accessKey}` }),
    ).toBeVisible();
  });

  test('TD-E-17 an expired session sends to the login and back to the same route', async ({
    page,
    context,
    api,
  }) => {
    const bucket = uniqueName('e2e-expiry');
    await api.createBucket(bucket);
    await api.putObject(bucket, 'docs/readme.txt', 'hello');
    try {
      await page.goto('/login');
      await loginWithForm(page, rootCredentials);
      const route = `/buckets/${bucket}/browse/docs/`;
      await page.goto(route);
      await expect(page.getByRole('button', { name: 'readme.txt', exact: true })).toBeVisible();

      // The session ends while the console is open (the cookies are gone,
      // as when the token expires): the next request answers 401.
      await context.clearCookies();
      await page.getByRole('button', { name: 'Refresh' }).click();
      await expect(page).toHaveURL(/\/login$/);
      await expect(page.getByText('Your session has expired.', { exact: false })).toBeVisible();

      await loginWithForm(page, rootCredentials);
      await expect(page).toHaveURL(new RegExp(`${route}$`));
      await expect(page.getByRole('button', { name: 'readme.txt', exact: true })).toBeVisible();
    } finally {
      await api.deleteBucket(bucket);
    }
  });
});
