import { configure, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { createRoutes } from '@/router';
import { isApiError, type Services } from '@/services';
import { createMockServices } from '@/services/mock';
import { renderRoutes } from '@/test/render';

import { cacheRoutes } from '../cacheRoutes';

const routes = cacheRoutes();

function admin(options: Parameters<typeof createMockServices>[0] = {}) {
  return createMockServices({ initialAccessKey: 'nexoadmin', ...options });
}

function open(path: string, services: Services = admin()) {
  const result = renderRoutes(routes, { initialEntries: [path], services });
  return { ...result, user: userEvent.setup() };
}

/** A user whose only permission on the cache is admin:InvalidateCache (TD-C-06). */
function deployer() {
  return createMockServices({
    accounts: { nexoadmin: 'nexoadmin', deployer: 'deployer-secret' },
    policies: [
      {
        name: 'cache-purge',
        document: {
          Version: '2012-10-17',
          Statement: [
            { Effect: 'Allow', Action: ['admin:InvalidateCache'], Resource: ['arn:aws:s3:::*'] },
          ],
        },
        builtIn: false,
        updatedAt: '2026-09-01T00:00:00Z',
      },
    ],
    users: [
      {
        accessKey: 'deployer',
        status: 'enabled',
        policyNames: ['cache-purge'],
        groups: [],
        createdAt: '2026-09-01T00:00:00Z',
        updatedAt: '2026-09-01T00:00:00Z',
      },
    ],
    groups: [],
    initialAccessKey: 'deployer',
  });
}

// The pages are lazy chunks: under the load of the whole suite they take
// longer to appear than the default waits.
configure({ asyncUtilTimeout: 5_000 });

describe('cache pages', { timeout: 30_000 }, () => {
  // covers: D-CCH-01
  it('D-CCH-01 shows the tiers, the hit ratio, the bandwidth saved and the top domains', async () => {
    const { router } = open('/cache');
    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/cache/overview');
    });
    expect(await screen.findByRole('region', { name: 'Hit ratio' })).toHaveTextContent('%');
    expect(screen.getByRole('region', { name: 'Origin bandwidth saved' })).toBeInTheDocument();
    expect(screen.getByRole('region', { name: 'Memory (L1)' })).toHaveTextContent('of 1 GiB');
    expect(screen.getByRole('region', { name: 'Disk (L2)' })).toHaveTextContent('of 40 GiB');
    const nodes = screen.getByRole('table', { name: 'Cache of the nodes' });
    expect(within(nodes).getAllByRole('row')).toHaveLength(5);
    const domains = screen.getByRole('table', { name: 'Top domains' });
    const rows = within(domains).getAllByRole('row');
    // The busiest domain first, with a link to the cache of its distribution.
    expect(rows[1]).toHaveTextContent('www.example.com');
    expect(rows[1]).toHaveTextContent('94.1%');
    expect(within(rows[1] as HTMLElement).getByRole('link', { name: 'website' })).toHaveAttribute(
      'href',
      '/distributions/website/cache',
    );
    // D-CCH-08: the charts per domain (a lazy chunk) and the top objects.
    expect(await screen.findByText('Hit ratio per domain')).toBeInTheDocument();
    expect(screen.getByText('Answers per domain by X-Cache')).toBeInTheDocument();
    expect(screen.getByText('Bytes served: cache vs origin')).toBeInTheDocument();
    expect(screen.getByRole('table', { name: 'Most requested objects' })).toHaveTextContent(
      'www.example.com/index.html',
    );
  });

  // covers: D-CCH-01
  it('D-CCH-01 filters the statistics by distribution', async () => {
    const { user } = open('/cache/overview');
    await screen.findByRole('table', { name: 'Top domains' });
    await user.click(screen.getByRole('textbox', { name: 'Statistics of' }));
    await user.click(await screen.findByRole('option', { name: 'download' }));
    const domains = await screen.findByRole('table', { name: 'Domains' });
    expect(within(domains).getAllByRole('row')).toHaveLength(2);
    expect(domains).toHaveTextContent('download.example.com');
  });

  // covers: D-CCH-03
  it('TD-C-02 previews the Cache-Control live and validates min ≤ default ≤ max', async () => {
    const services = admin();
    const { user, router } = open('/cache/policies/-/new', services);
    const form = await screen.findByRole('form', { name: 'New cache policy' });
    await user.type(within(form).getByRole('textbox', { name: 'Name' }), 'short');
    const min = within(form).getByRole('textbox', { name: 'TTL min' });
    await user.clear(min);
    await user.type(min, '2h');
    expect(
      await within(form).findByText('The minimum 2h is above the default 1h: min ≤ default ≤ max.'),
    ).toBeInTheDocument();
    const create = within(form).getByRole('button', { name: 'Create policy' });
    expect(create).toBeDisabled();
    const max = within(form).getByRole('textbox', { name: 'TTL max' });
    await user.clear(max);
    await user.type(max, '30m');
    expect(
      await within(form).findByText(
        'The default 1h is above the maximum 30m: min ≤ default ≤ max.',
      ),
    ).toBeInTheDocument();
    await user.clear(max);
    await user.type(max, '30d');
    const def = within(form).getByRole('textbox', { name: 'TTL default' });
    await user.clear(def);
    await user.type(def, '1d');
    await waitFor(() => {
      expect(create).toBeEnabled();
    });

    // The Cache-Control of the clients, previewed as it is built.
    const preview = screen.getByRole('region', { name: 'Preview' });
    expect(within(preview).getByTestId('cache-control-preview')).toHaveTextContent(
      'the clients receive the Cache-Control of each object',
    );
    expect(preview).toHaveTextContent('Fresh in the cache for 1d (between 2h and 30d).');
    await user.click(
      within(form).getByRole('switch', { name: /^Send a Cache-Control of the policy/ }),
    );
    expect(within(preview).getByTestId('cache-control-preview')).toHaveTextContent(
      'public, max-age=3600',
    );
    await user.click(
      within(form).getByRole('button', { name: 'Versioned assets (1 year, immutable)' }),
    );
    expect(within(preview).getByTestId('cache-control-preview')).toHaveTextContent(
      'public, max-age=31536000, immutable',
    );
    await user.click(within(form).getByRole('checkbox', { name: 'immutable' }));
    expect(within(preview).getByTestId('cache-control-preview')).toHaveTextContent(
      /^public, max-age=31536000$/,
    );
    // The summary of the server once the policy is valid.
    await waitFor(() => {
      expect(within(preview).getByTestId('policy-summary')).toHaveTextContent('fresh 1d');
    });

    await user.click(create);
    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/cache/policies/short');
    });
    const stored = await services.cache.getPolicy('short');
    expect(stored.ttl).toMatchObject({ min: '2h', default: '1d', max: '30d' });
    expect(stored.browserCacheControl).toBe('public, max-age=31536000');
  });

  // covers: D-CCH-03
  it('D-CCH-03 shows a built-in policy read-only and duplicates it', async () => {
    const { user, router } = open('/cache/policies/aggressive');
    expect(await screen.findByRole('heading', { name: 'Policy aggressive' })).toBeInTheDocument();
    expect(screen.getByText('Built-in')).toBeInTheDocument();
    expect(screen.getByRole('textbox', { name: 'TTL default' })).toBeDisabled();
    expect(screen.queryByRole('button', { name: 'Save' })).not.toBeInTheDocument();
    await user.click(screen.getByRole('link', { name: 'Duplicate' }));
    await waitFor(() => {
      expect(router.state.location.search).toBe('?from=aggressive');
    });
    expect(await screen.findByRole('textbox', { name: 'Name' })).toHaveValue('aggressive-copy');
    expect(screen.getByRole('textbox', { name: 'TTL default' })).toHaveValue('1d');
  });

  // covers: D-CCH-03
  it('D-CCH-03 lists the policies and refuses to delete one in use', async () => {
    const services = admin({
      cachePolicies: [{ name: 'short', ttl: { default: '10m' } }],
      cacheSettings: { cache: { policy: 'short' } },
    });
    const { user } = open('/cache/policies', services);
    const table = await screen.findByRole('table', { name: 'Cache policies' });
    expect(within(table).getByRole('link', { name: 'short' })).toBeInTheDocument();
    expect(within(table).getAllByText('Built-in')).toHaveLength(5);
    await user.click(within(table).getByRole('button', { name: 'Delete short' }));
    const dialog = await screen.findByRole('dialog', { name: 'Delete the policy' });
    await user.click(within(dialog).getByRole('button', { name: 'Delete' }));
    expect(await within(dialog).findByText('cache policy short is in use')).toBeInTheDocument();
  });

  // covers: D-CCH-04, D-DST-08
  it('TD-C-03 follows an invalidation by glob node by node until completed', async () => {
    const services = admin({ cacheSteps: 2 });
    const { user } = open('/cache/invalidations', services);
    const form = await screen.findByRole('form', { name: 'Invalidate' });
    await user.click(within(form).getByRole('textbox', { name: 'Distribution' }));
    await user.click(await screen.findByRole('option', { name: 'website' }));
    await user.click(within(form).getByRole('radio', { name: 'Patterns' }));
    await user.type(within(form).getByRole('textbox', { name: 'Pattern 1' }), '/assets/**');
    await user.click(within(form).getByRole('textbox', { name: 'Only on the domain' }));
    await user.click(await screen.findByRole('option', { name: 'www.example.com' }));
    await user.click(within(form).getByRole('button', { name: 'Invalidate' }));

    const progress = await screen.findByRole('region', { name: /^Invalidation inv-/ });
    expect(progress).toHaveTextContent('domain www.example.com · glob /assets/**');
    expect(within(progress).getByLabelText(/^In progress: [1-3] of 4 nodes$/)).toBeInTheDocument();
    // Read again every second: one more node completes at every read.
    expect(
      await within(progress).findByLabelText('Completed: 4 of 4 nodes', undefined, {
        timeout: 8_000,
      }),
    ).toBeInTheDocument();
    const nodes = within(progress).getByRole('table');
    expect(within(nodes).getAllByText('Completed')).toHaveLength(4);
    const history = await screen.findByRole('table', { name: 'History of website' });
    expect(history).toHaveTextContent('glob /assets/**');
    expect(history).toHaveTextContent('nexoadmin');
  });

  // covers: D-CCH-05
  it('TD-C-04 shows the effective policy of a URL and the level of every setting', async () => {
    const services = admin({ cacheSteps: 0 });
    // A warm-up caches the object on the first node.
    await services.cache.warm('download', { paths: ['/db/2026-10-01.sql.gz'] });
    const { user } = open('/cache/inspector', services);
    const form = await screen.findByRole('form', { name: 'URL inspector' });
    await user.type(
      within(form).getByRole('textbox', { name: 'URL' }),
      'https://www.example.com/assets/app.js',
    );
    await user.click(within(form).getByRole('button', { name: 'Inspect' }));
    const result = await screen.findByRole('region', { name: 'How the cache serves the URL' });
    const policy = within(result).getByRole('table', { name: 'Effective policy of the URL' });
    const row = (name: string) => within(policy).getByRole('row', { name: new RegExp(`^${name}`) });
    expect(row('Policy')).toHaveTextContent('aggressive');
    expect(row('Policy')).toHaveTextContent('domain');
    expect(row('Lifetime in cache')).toHaveTextContent('365d (min 1d, max 365d)');
    expect(row('Lifetime in cache')).toHaveTextContent('path');
    expect(row('Cache-Control to the clients')).toHaveTextContent(
      'public, max-age=31536000, immutable',
    );
    expect(
      within(result).getByText(/v1\|h:www\.example\.com\|media\/prod\/assets\/app\.js/),
    ).toBeInTheDocument();
    const nodes = within(result).getByRole('table', { name: 'Entry of the URL on every node' });
    expect(within(nodes).getAllByText('Not cached')).toHaveLength(4);

    // A URL of a warmed object: cached on the first node.
    const url = within(form).getByRole('textbox', { name: 'URL' });
    await user.clear(url);
    await user.type(url, 'https://download.example.com/db/2026-10-01.sql.gz');
    await user.click(within(form).getByRole('button', { name: 'Inspect' }));
    await waitFor(() => {
      expect(
        screen.getByRole('table', { name: 'Entry of the URL on every node' }),
      ).toHaveTextContent('Cached on disk');
    });
    // The preview domain disables the cache: the bypass and its level.
    await user.clear(url);
    await user.type(url, 'https://pr-1.preview.example.com/');
    await user.click(within(form).getByRole('button', { name: 'Inspect' }));
    expect(await screen.findByText('The cache is bypassed')).toBeInTheDocument();
  });

  // covers: D-CCH-06
  it('D-CCH-06 warms a domain and shows the progress', async () => {
    const { user } = open('/cache/warm', admin({ cacheSteps: 1 }));
    const form = await screen.findByRole('form', { name: 'Warm-up' });
    await user.click(within(form).getByRole('textbox', { name: 'Distribution' }));
    await user.click(await screen.findByRole('option', { name: 'download' }));
    await user.type(
      within(form).getByRole('textbox', { name: 'URLs or paths' }),
      '/db/2026-10-01.sql.gz{Enter}https://download.example.com/missing.bin',
    );
    await user.click(within(form).getByRole('button', { name: 'Start the warm-up' }));
    const progress = await screen.findByRole('region', { name: /^Warm-up warm-/ });
    expect(progress).toHaveTextContent('Domain download.example.com');
    await waitFor(
      () => {
        expect(progress.querySelector('[data-state="completed"]')).not.toBeNull();
      },
      { timeout: 8_000 },
    );
    expect(progress).toHaveTextContent('1 objects not found');
  });

  // covers: D-CCH-06
  it('D-CCH-06 warms every object under a prefix', async () => {
    const services = admin({ cacheSteps: 1 });
    const warm = vi.spyOn(services.cache, 'warm');
    const { user } = open('/cache/warm', services);
    const form = await screen.findByRole('form', { name: 'Warm-up' });
    await user.click(within(form).getByRole('textbox', { name: 'Distribution' }));
    await user.click(await screen.findByRole('option', { name: 'download' }));
    await user.click(within(form).getByRole('radio', { name: 'Prefix' }));
    const start = within(form).getByRole('button', { name: 'Start the warm-up' });
    expect(start).toBeDisabled();
    await user.type(within(form).getByRole('textbox', { name: 'Path prefix' }), '/db/');
    await user.click(start);
    expect(warm).toHaveBeenCalledWith('download', { prefix: '/db/' });
    const progress = await screen.findByRole('region', { name: /^Warm-up warm-/ });
    await waitFor(
      () => {
        expect(progress.querySelector('[data-state="completed"]')).not.toBeNull();
      },
      { timeout: 8_000 },
    );
    // The two dumps under db/ of the origin bucket.
    expect(progress).toHaveTextContent('2/2 objects');
  });

  it('saves the settings of the cluster with If-Match', async () => {
    const services = admin();
    const { user } = open('/cache/settings', services);
    const section = await screen.findByRole('region', { name: 'Settings of the cluster' });
    await user.click(
      within(section).getByRole('switch', { name: /^Add X-Cache-Node to every answer/ }),
    );
    await user.click(within(section).getByRole('button', { name: 'Save' }));
    await waitFor(async () => {
      expect((await services.cache.getSettings()).nodeHeader).toBe(true);
    });
  });

  // covers: D-CCH-04, D-UX-02
  it('TD-C-06 lets a user with admin:InvalidateCache alone invalidate, not change the policies', async () => {
    const services = deployer();
    const { user, router } = open('/cache', services);
    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/cache/invalidations');
    });
    const nav = await screen.findByRole('navigation', { name: 'Cache sections' });
    expect(
      within(nav)
        .getAllByRole('link')
        .map((l) => l.textContent),
    ).toEqual(['Invalidations', 'Warm-up']);
    // The distributions cannot be listed: the id is typed.
    const form = await screen.findByRole('form', { name: 'Invalidate' });
    await user.type(within(form).getByRole('textbox', { name: 'Distribution' }), 'website');
    await user.type(within(form).getByRole('textbox', { name: 'Paths' }), '/index.html');
    await user.click(within(form).getByRole('button', { name: 'Invalidate' }));
    expect(await screen.findByRole('region', { name: /^Invalidation inv-/ })).toHaveTextContent(
      'paths /index.html',
    );

    // The policies stay out of reach, in the console and on the server.
    await router.navigate('/cache/policies');
    expect(
      await screen.findByText('You may not open Policies: admin:ManageCache is required.'),
    ).toBeInTheDocument();
    await router.navigate('/cache/policies/-/new');
    expect(
      await screen.findByText('You do not have the permission admin:ManageCache.'),
    ).toBeInTheDocument();
    const refused = await services.cache
      .createPolicy({ name: 'x', ttl: { default: '1h' } })
      .catch((e: unknown) => e);
    expect(isApiError(refused) && refused.status).toBe(403);
  });

  // covers: D-CCH-04, D-UX-02
  it('TD-C-06 shows the cache entry of the menu to a user who may invalidate', async () => {
    renderRoutes(createRoutes(), { initialEntries: ['/cache'], services: deployer() });
    const nav = await screen.findByRole('navigation', { name: 'Main navigation' });
    await waitFor(() => {
      expect(within(nav).queryByRole('link', { name: 'Users' })).not.toBeInTheDocument();
    });
    expect(within(nav).getByRole('link', { name: 'Cache' })).toBeInTheDocument();
  });
});
