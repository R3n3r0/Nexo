import { configure, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';

import type { Services } from '@/services';
import { createMockServices } from '@/services/mock';
import { renderRoutes } from '@/test/render';

import { distributionRoutes } from '../distributionRoutes';

const routes = distributionRoutes();

function open(
  path: string,
  services: Services = createMockServices({ initialAccessKey: 'nexoadmin' }),
) {
  const result = renderRoutes(routes, { initialEntries: [path], services });
  return { ...result, user: userEvent.setup() };
}

/** The row of a domain in the domains table. */
async function domainRow(host: string): Promise<HTMLElement> {
  const table = await screen.findByRole('table', { name: 'Domains' });
  const row = within(table)
    .getAllByRole('row')
    .find((r) => r.getAttribute('data-domain') === host);
  if (row === undefined) throw new Error(`no row for ${host}`);
  return row;
}

/** D-CCH-02, D-CCH-07, D-CCH-08: the cache tab of a distribution, on the mocks. */
// The pages are lazy chunks: under the load of the whole suite they take
// longer to appear than the default waits.
configure({ asyncUtilTimeout: 5_000 });

describe('distribution cache tab', { timeout: 30_000 }, () => {
  it('D-CCH-02 shows every domain with its effective policy, the level setting it, usage and hit ratio', async () => {
    open('/distributions/sito-web/cache');
    const www = await domainRow('www.example.com');
    expect(within(www).getByText('aggressive')).toBeInTheDocument();
    expect(within(www).getAllByText('domain')).toHaveLength(1);
    expect(within(www).getByText('20GiB')).toBeInTheDocument();
    expect(within(www).getByText('Overridden')).toBeInTheDocument();
    // The statistics of the domain: space in cache and hit ratio.
    await waitFor(() => {
      expect(www.querySelector('[data-column="hit-ratio"]')).toHaveTextContent('94.1%');
    });
    const apex = await domainRow('example.com');
    expect(within(apex).getByText('standard')).toBeInTheDocument();
    expect(within(apex).getAllByText('distribution')).toHaveLength(2);
    expect(within(apex).getByText('Inherits')).toBeInTheDocument();
    // The wildcard of the previews: cache off for every preview host.
    const previews = await domainRow('*.preview.example.com');
    expect(previews.querySelector('[data-enabled]')).toHaveAttribute('data-enabled', 'false');
    // The paths: the first match wins.
    const paths = screen.getByRole('table', { name: 'Paths' });
    expect(paths).toHaveTextContent('glob /assets/**');
    expect(paths).toHaveTextContent('TTL 365d');
    expect(paths).toHaveTextContent('regex ^/api/');
    expect(paths).toHaveTextContent('cache off');
  });

  it('TD-C-01 overrides the policy of a domain and saves it with If-Match', async () => {
    const services = createMockServices({ initialAccessKey: 'nexoadmin' });
    const { user } = open('/distributions/sito-web/cache', services);
    const apex = await domainRow('example.com');
    await user.click(
      within(apex).getByRole('button', { name: 'Override the cache of example.com' }),
    );
    const dialog = await screen.findByRole('dialog', { name: 'Cache of example.com' });
    expect(within(dialog).getByText(/^Inherited: standard\./)).toBeInTheDocument();
    await user.click(within(dialog).getByRole('textbox', { name: 'Policy' }));
    await user.click(await screen.findByRole('option', { name: 'immutable-assets' }));
    await user.type(within(dialog).getByRole('textbox', { name: 'Quota' }), '5GiB');
    await user.click(within(dialog).getByRole('button', { name: 'Apply' }));
    await waitFor(() => {
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    });
    const row = await domainRow('example.com');
    expect(within(row).getByText('immutable-assets')).toBeInTheDocument();
    expect(within(row).getByText('Overridden')).toBeInTheDocument();
    const bar = screen.getByRole('region', { name: 'Unsaved cache settings' });
    await user.click(within(bar).getByRole('button', { name: 'Save' }));
    await waitFor(() => {
      expect(
        screen.queryByRole('region', { name: 'Unsaved cache settings' }),
      ).not.toBeInTheDocument();
    });
    const stored = await services.cache.getDistribution('sito-web');
    expect(stored.domains).toContainEqual({
      host: 'example.com',
      cache: { policy: 'immutable-assets', quota: '5GiB' },
    });
    // The server applies it: the effective policy of the domain.
    const effective = await services.cache.effective('sito-web', {
      host: 'example.com',
      path: '/index.html',
    });
    expect(effective).toMatchObject({ policyName: 'immutable-assets', policyLevel: 'domain' });
  });

  it('reports a concurrent change of the cache settings (412) and reloads them', async () => {
    const services = createMockServices({ initialAccessKey: 'nexoadmin' });
    const { user } = open('/distributions/sito-web/cache', services);
    const apex = await domainRow('example.com');
    // Another console changes the settings meanwhile.
    const current = await services.cache.getDistribution('sito-web');
    await services.cache.updateDistribution({ ...current, cache: { policy: 'aggressive' } });
    await user.click(
      within(apex).getByRole('button', { name: 'Override the cache of example.com' }),
    );
    const dialog = await screen.findByRole('dialog', { name: 'Cache of example.com' });
    await user.click(within(dialog).getByRole('radio', { name: 'Disabled' }));
    await user.click(within(dialog).getByRole('button', { name: 'Apply' }));
    const bar = await screen.findByRole('region', { name: 'Unsaved cache settings' });
    await user.click(within(bar).getByRole('button', { name: 'Save' }));
    expect(await within(bar).findByText('Changed meanwhile')).toBeInTheDocument();
    await user.click(within(bar).getByRole('button', { name: 'Reload' }));
    const row = await domainRow('example.com');
    await waitFor(() => {
      expect(within(row).getByText('aggressive')).toBeInTheDocument();
    });
  });

  it('adds a path override and refuses an inverted TTL', async () => {
    const { user } = open('/distributions/sito-web/cache');
    await screen.findByRole('table', { name: 'Paths' });
    await user.click(screen.getByRole('button', { name: 'Add a path override' }));
    const dialog = await screen.findByRole('dialog', { name: 'Add a path override' });
    await user.type(within(dialog).getByRole('textbox', { name: 'Path pattern' }), '/media/**');
    await user.type(within(dialog).getByRole('textbox', { name: 'TTL min' }), '2d');
    await user.type(within(dialog).getByRole('textbox', { name: 'TTL default' }), '1d');
    expect(
      await within(dialog).findByText(
        'The minimum 2d is above the default 1d: min ≤ default ≤ max.',
      ),
    ).toBeInTheDocument();
    expect(within(dialog).getByRole('button', { name: 'Apply' })).toBeDisabled();
    await user.clear(within(dialog).getByRole('textbox', { name: 'TTL min' }));
    await user.click(within(dialog).getByRole('button', { name: 'Apply' }));
    const paths = screen.getByRole('table', { name: 'Paths' });
    await waitFor(() => {
      expect(paths).toHaveTextContent('glob /media/**');
    });
    expect(paths).toHaveTextContent('TTL 1d');
  });

  it('D-CCH-08 charts the answers per domain of the distribution', async () => {
    open('/distributions/sito-web/cache?section=statistics');
    const domains = await screen.findByRole('table', { name: 'Domains' });
    expect(within(domains).getAllByRole('row')).toHaveLength(4);
    expect(await screen.findByText('Hit ratio per domain')).toBeInTheDocument();
    expect(screen.getByRole('table', { name: 'Most missed objects' })).toHaveTextContent(
      'example.com/',
    );
  });

  it('D-CCH-07 adds a purge webhook, tests it and removes it', async () => {
    const services = createMockServices({ initialAccessKey: 'nexoadmin' });
    const { user } = open('/distributions/sito-web/cache?section=webhooks', services);
    const section = await screen.findByRole('region', { name: 'Purge webhooks' });
    const table = await within(section).findByRole('table', { name: 'Purge webhooks' });
    expect(table).toHaveTextContent('cloudflare-www');

    await user.click(within(section).getByRole('button', { name: 'Add webhook' }));
    const dialog = await screen.findByRole('dialog', { name: 'New purge webhook' });
    await user.type(within(dialog).getByRole('textbox', { name: 'ID' }), 'edge');
    await user.click(within(dialog).getByRole('textbox', { name: 'Type' }));
    await user.click(await screen.findByRole('option', { name: 'Generic' }));
    const add = within(dialog).getByRole('button', { name: 'Add webhook' });
    expect(add).toBeDisabled();
    await user.type(
      within(dialog).getByRole('textbox', { name: 'URL' }),
      'http://127.0.0.1:9/purge',
    );
    await user.click(add);
    const row = await within(section).findByRole('row', { name: /^edge/ });
    await user.click(within(row).getByRole('button', { name: 'Test edge' }));
    expect(await within(row).findByText(/^Failed: connection refused/)).toBeInTheDocument();
    const cf = within(table).getByRole('row', { name: /^cloudflare-www/ });
    await user.click(within(cf).getByRole('button', { name: 'Test cloudflare-www' }));
    expect(await within(cf).findByText('OK (200, 42 ms)')).toBeInTheDocument();

    await user.click(within(row).getByRole('button', { name: 'Delete edge' }));
    const confirm = await screen.findByRole('dialog', { name: 'Delete the webhook' });
    await user.click(within(confirm).getByRole('button', { name: 'Delete' }));
    await waitFor(async () => {
      expect((await services.cache.webhooks('sito-web')).map((w) => w.id)).toEqual([
        'cloudflare-www',
      ]);
    });
  });

  it('D-DST-08 invalidates a path of a domain from the distribution', async () => {
    const services = createMockServices({ initialAccessKey: 'nexoadmin', cacheSteps: 0 });
    const { user } = open('/distributions/sito-web/cache?section=invalidations', services);
    const form = await screen.findByRole('form', { name: 'Invalidate' });
    await user.type(within(form).getByRole('textbox', { name: 'Paths' }), '/index.html');
    await user.click(within(form).getByRole('switch', { name: /^Soft purge/ }));
    await user.click(within(form).getByRole('button', { name: 'Mark expired' }));
    const history = await screen.findByRole('table', { name: 'History of sito-web' });
    expect(history).toHaveTextContent('paths /index.html · soft');
    expect(within(history).getByLabelText('Completed: 4 of 4 nodes')).toBeInTheDocument();
  });
});
