import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Navigate, useLocation } from 'react-router';
import { describe, expect, it, vi } from 'vitest';

import { ApiError } from '@/services';
import { createMockServices } from '@/services/mock';
import { renderRoutes } from '@/test/render';
import { UploadProvider } from '@/upload/UploadProvider';

import { ObjectBrowser } from '../objects/ObjectBrowser';
import { BucketAccessTab } from './BucketAccessTab';
import { BucketDetailPage } from './BucketDetailPage';
import { BucketSettingsTab } from './BucketSettingsTab';

function Location() {
  return <div data-testid="location">{useLocation().pathname}</div>;
}

const routes = [
  {
    path: '/buckets/:bucket',
    element: (
      <UploadProvider>
        <Location />
        <BucketDetailPage />
      </UploadProvider>
    ),
    children: [
      { index: true, element: <Navigate to="browse/" replace /> },
      { path: 'browse/*', element: <ObjectBrowser /> },
      { path: 'settings', element: <BucketSettingsTab /> },
      { path: 'access', element: <BucketAccessTab /> },
    ],
  },
  { path: '/buckets', element: <Location /> },
];

const admin = () => createMockServices({ initialAccessKey: 'nexoadmin' });

function readOnly() {
  return createMockServices({
    accounts: { nexoadmin: 'nexoadmin', reader: 'reader-secret' },
    users: [
      {
        accessKey: 'reader',
        status: 'enabled',
        policyNames: ['readonly'],
        groups: [],
        createdAt: '2026-09-01T00:00:00Z',
        updatedAt: '2026-09-01T00:00:00Z',
      },
    ],
    groups: [],
    initialAccessKey: 'reader',
  });
}

describe('BucketDetailPage', () => {
  it('opens the objects tab by default and switches tabs through the route', async () => {
    renderRoutes(routes, { initialEntries: ['/buckets/media'], services: admin() });
    await waitFor(() => {
      expect(screen.getByTestId('location')).toHaveTextContent('/buckets/media/browse/');
    });
    expect(await screen.findByRole('heading', { name: 'media' })).toBeInTheDocument();
    expect(screen.getByText('70 GiB · 11,500 objects')).toBeInTheDocument();
    const user = userEvent.setup();
    await user.click(screen.getByRole('tab', { name: 'Settings' }));
    expect(screen.getByTestId('location')).toHaveTextContent('/buckets/media/settings');
    await user.click(screen.getByRole('tab', { name: 'Access' }));
    expect(screen.getByTestId('location')).toHaveTextContent('/buckets/media/access');
    expect(screen.getByRole('tab', { name: 'Access' })).toHaveAttribute('aria-selected', 'true');
  });

  it('tells when the bucket does not exist', async () => {
    renderRoutes(routes, { initialEntries: ['/buckets/nope/settings'], services: admin() });
    expect(await screen.findByRole('alert')).toHaveTextContent('The bucket nope does not exist.');
  });

  it('stays open on a 403 so that a home directory can be browsed', async () => {
    const svc = admin();
    vi.spyOn(svc.buckets, 'get').mockRejectedValue(
      new ApiError({ status: 403, code: 'access_denied', title: 'Forbidden' }),
    );
    renderRoutes(routes, { initialEntries: ['/buckets/media/browse/'], services: svc });
    expect(await screen.findByText('You may use part of this bucket only.')).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: 'media' })).toBeInTheDocument();
    expect(screen.getByRole('tab', { name: 'Objects' })).toHaveAttribute('aria-selected', 'true');
    expect(screen.queryByText('Could not load the bucket.')).not.toBeInTheDocument();
  });
});

describe('BucketSettingsTab (D-BKT-02)', () => {
  it('changes versioning, quota and tags', async () => {
    const services = admin();
    const setVersioning = vi.spyOn(services.buckets, 'setVersioning');
    const setQuota = vi.spyOn(services.buckets, 'setQuota');
    const setTags = vi.spyOn(services.buckets, 'setTags');
    renderRoutes(routes, { initialEntries: ['/buckets/media/settings'], services });
    const user = userEvent.setup();

    const versioning = await screen.findByRole('region', { name: 'Versioning' });
    expect(await within(versioning).findByText('Never enabled')).toBeInTheDocument();
    await user.click(within(versioning).getByRole('radio', { name: 'Enabled' }));
    await waitFor(() => {
      expect(setVersioning).toHaveBeenCalledWith('media', 'enabled');
    });
    expect(
      await within(versioning).findByText('Enabled', { selector: '.mantine-Badge-label' }),
    ).toBeInTheDocument();

    const quota = screen.getByRole('region', { name: 'Quota' });
    const limit = within(quota).getByRole('textbox', { name: 'Hard quota' });
    await waitFor(() => {
      expect(limit).toHaveValue('100');
    });
    await user.clear(limit);
    await user.type(limit, '50');
    await user.click(within(quota).getByRole('button', { name: 'Save' }));
    await waitFor(() => {
      expect(setQuota).toHaveBeenCalledWith('media', 50 * 1024 ** 3);
    });

    const tags = screen.getByRole('region', { name: 'Tags' });
    await user.click(await within(tags).findByRole('button', { name: 'Add tag' }));
    await user.type(within(tags).getByRole('textbox', { name: 'Key of tag 1' }), 'env');
    await user.type(within(tags).getByRole('textbox', { name: 'Value of tag 1' }), 'prod');
    await user.click(within(tags).getByRole('button', { name: 'Add tag' }));
    await user.type(within(tags).getByRole('textbox', { name: 'Key of tag 2' }), 'env');
    expect(within(tags).getByRole('alert')).toHaveTextContent('Every key must be unique');
    expect(within(tags).getByRole('button', { name: 'Save tags' })).toBeDisabled();
    const removeButtons = within(tags).getAllByRole('button', { name: 'Remove tag env' });
    await user.click(removeButtons[1]!);
    await user.click(within(tags).getByRole('button', { name: 'Save tags' }));
    await waitFor(() => {
      expect(setTags).toHaveBeenCalledWith('media', { env: 'prod' });
    });
  });

  it('shows object lock and keeps versioning on a locked bucket', async () => {
    renderRoutes(routes, { initialEntries: ['/buckets/logs-archive/settings'], services: admin() });
    const lock = await screen.findByRole('region', { name: 'Object lock' });
    expect(await within(lock).findByText('GOVERNANCE · 30 days')).toBeInTheDocument();
    expect(
      within(lock).getByText(/default retention applies to the new objects/),
    ).toBeInTheDocument();
    expect(
      screen.getByText('Versioning cannot be suspended on a bucket with object lock.'),
    ).toBeInTheDocument();
    const encryption = screen.getByRole('region', { name: 'Encryption' });
    expect(
      await within(encryption).findByText('Not configured', { selector: '.mantine-Badge-label' }),
    ).toBeInTheDocument();
  });

  // Regression (D-BKT-02): encryption and object lock were read-only.
  it('sets the default encryption and the object lock retention', async () => {
    const services = admin();
    const setEncryption = vi.spyOn(services.buckets, 'setEncryption');
    const setObjectLock = vi.spyOn(services.buckets, 'setObjectLock');
    renderRoutes(routes, { initialEntries: ['/buckets/logs-archive/settings'], services });
    const user = userEvent.setup();

    const encryption = await screen.findByRole('region', { name: 'Encryption' });
    await user.click(
      await within(encryption).findByRole('textbox', { name: 'Default encryption' }),
    );
    await user.click(await screen.findByRole('option', { name: 'SSE-KMS' }));
    await user.type(within(encryption).getByRole('textbox', { name: /KMS key ID/ }), 'nexo-key');
    await user.click(within(encryption).getByRole('button', { name: 'Save' }));
    await waitFor(() => {
      expect(setEncryption).toHaveBeenCalledWith('logs-archive', {
        algorithm: 'aws:kms',
        kmsKeyId: 'nexo-key',
      });
    });
    expect(await within(encryption).findByText('SSE-KMS (nexo-key)')).toBeInTheDocument();

    const lock = screen.getByRole('region', { name: 'Object lock' });
    await within(lock).findByText('GOVERNANCE · 30 days');
    await user.click(within(lock).getByRole('textbox', { name: 'Default mode' }));
    await user.click(await screen.findByRole('option', { name: 'COMPLIANCE' }));
    const period = within(lock).getByRole('textbox', { name: 'Default retention' });
    await user.clear(period);
    await user.type(period, '2');
    await user.click(within(lock).getByRole('textbox', { name: 'Retention unit' }));
    await user.click(await screen.findByRole('option', { name: 'years' }));
    await user.click(within(lock).getByRole('button', { name: 'Save' }));
    await waitFor(() => {
      expect(setObjectLock).toHaveBeenCalledWith('logs-archive', {
        enabled: true,
        mode: 'COMPLIANCE',
        years: 2,
      });
    });
    expect(await within(lock).findByText('COMPLIANCE · 2 years')).toBeInTheDocument();

    // Removing the default encryption.
    await user.click(within(encryption).getByRole('textbox', { name: 'Default encryption' }));
    await user.click(await screen.findByRole('option', { name: 'Not configured' }));
    await user.click(within(encryption).getByRole('button', { name: 'Save' }));
    await waitFor(() => {
      expect(setEncryption).toHaveBeenLastCalledWith('logs-archive', null);
    });
  });

  it('enables object lock only at the creation of the bucket', async () => {
    renderRoutes(routes, { initialEntries: ['/buckets/backups/settings'], services: admin() });
    const lock = await screen.findByRole('region', { name: 'Object lock' });
    expect(
      await within(lock).findByText('Object lock can only be enabled when the bucket is created.'),
    ).toBeInTheDocument();
    expect(
      within(lock).queryByRole('button', { name: 'Enable object lock' }),
    ).not.toBeInTheDocument();
  });

  it('disables the changes for a read-only user', async () => {
    renderRoutes(routes, { initialEntries: ['/buckets/media/settings'], services: readOnly() });
    const quota = await screen.findByRole('region', { name: 'Quota' });
    await waitFor(() => {
      expect(within(quota).getByRole('button', { name: 'Save' })).toBeDisabled();
    });
    expect(within(quota).getByRole('textbox', { name: 'Hard quota' })).toBeDisabled();
  });
});

describe('BucketAccessTab (D-POL-01)', () => {
  it('applies and removes an anonymous preset on a prefix', async () => {
    const services = admin();
    renderRoutes(routes, { initialEntries: ['/buckets/media/access'], services });
    const section = await screen.findByRole('region', { name: 'Anonymous access' });
    expect(
      await within(section).findByText('The bucket is private: anonymous requests are denied.'),
    ).toBeInTheDocument();
    const user = userEvent.setup();
    await user.type(within(section).getByRole('textbox', { name: /Prefix/ }), 'public/');
    await user.click(within(section).getByRole('button', { name: 'Apply' }));

    const rules = await within(section).findByRole('table', { name: 'Anonymous access rules' });
    expect(within(rules).getByText('public/')).toBeInTheDocument();
    expect(within(rules).getByText('Download')).toBeInTheDocument();
    expect(
      within(section).getByText('Public', { selector: '.mantine-Badge-label' }),
    ).toBeInTheDocument();
    // The policy editor shows the generated statements.
    await waitFor(() => {
      expect(
        screen.getByRole<HTMLTextAreaElement>('textbox', { name: 'Bucket policy (JSON)' }).value,
      ).toContain('arn:aws:s3:::media/public/*');
    });

    await user.click(within(rules).getByRole('button', { name: 'Make public/ private' }));
    expect(
      await within(section).findByText('The bucket is private: anonymous requests are denied.'),
    ).toBeInTheDocument();
    expect((await services.buckets.getAccess('media')).rules).toEqual([]);
  });

  it('shows the located errors of a malformed policy', async () => {
    const services = admin();
    vi.spyOn(services.buckets, 'setPolicy').mockRejectedValue(
      new ApiError({
        title: 'Bad Request',
        status: 400,
        code: 'malformed_policy',
        errors: [{ statement: 1, line: 4, column: 7, message: 'unknown action s3:Nope' }],
      }),
    );
    renderRoutes(routes, { initialEntries: ['/buckets/media/access'], services });
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Start from a template' }));
    const editor = screen.getByRole<HTMLTextAreaElement>('textbox', {
      name: 'Bucket policy (JSON)',
    });
    expect(editor.value).toContain('"PublicRead"');
    await user.click(screen.getByRole('button', { name: 'Save policy' }));
    expect(await screen.findByText(/unknown action s3:Nope/)).toBeInTheDocument();
  });

  it('saves and deletes a valid policy', async () => {
    const services = admin();
    renderRoutes(routes, { initialEntries: ['/buckets/media/access'], services });
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Start from a template' }));
    await user.click(screen.getByRole('button', { name: 'Save policy' }));
    await waitFor(async () => {
      expect(await services.buckets.getPolicy('media')).toContain('PublicRead');
    });
    await waitFor(() => {
      expect(screen.getByRole('button', { name: 'Delete policy' })).toBeEnabled();
    });
    await user.click(screen.getByRole('button', { name: 'Delete policy' }));
    await waitFor(async () => {
      expect(await services.buckets.getPolicy('media')).toBeNull();
    });
  });

  it('is read-only without s3:PutBucketPolicy', async () => {
    renderRoutes(routes, { initialEntries: ['/buckets/media/access'], services: readOnly() });
    const section = await screen.findByRole('region', { name: 'Anonymous access' });
    await waitFor(() => {
      expect(within(section).getByRole('button', { name: 'Apply' })).toBeDisabled();
    });
    expect(within(section).getByRole('textbox', { name: /Prefix/ })).toBeDisabled();
  });
});
