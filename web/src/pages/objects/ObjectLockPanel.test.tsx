import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import type { PermissionCheck } from '@/auth/usePermissions';
import { createMockServices } from '@/services/mock';
import { renderWithProviders } from '@/test/render';

import { ObjectLockPanel } from './ObjectLockPanel';
import { VersionDeleteDialog } from './VersionDeleteDialog';

const bucket = 'logs-archive';
const key = '2026/10/01/app.log';
const day = 86_400_000;

/** Permissions granting everything but the listed actions. */
function permsWithout(...denied: string[]): PermissionCheck {
  return {
    loading: false,
    ready: true,
    permissions: undefined,
    can: (action) => !denied.includes(action),
  };
}

async function servicesWith(mode: 'GOVERNANCE' | 'COMPLIANCE', days: number) {
  const services = createMockServices({ initialAccessKey: 'nexoadmin' });
  await services.objects.setRetention(bucket, key, {
    mode,
    retainUntil: new Date(Date.now() + days * day).toISOString(),
  });
  return services;
}

/** D-OBJ-06, TS-U-12: retention (with the governance bypass) and legal hold. */
describe('ObjectLockPanel', () => {
  it('shortens a GOVERNANCE retention only with the bypass', async () => {
    const services = await servicesWith('GOVERNANCE', 10);
    const setRetention = vi.spyOn(services.objects, 'setRetention');
    const user = userEvent.setup();
    renderWithProviders(
      <ObjectLockPanel bucket={bucket} objectKey={key} perms={permsWithout()} />,
      {
        services,
      },
    );
    const retention = await screen.findByRole('region', { name: 'Retention' });
    expect(await within(retention).findByText(/^GOVERNANCE until/)).toBeInTheDocument();

    await user.click(within(retention).getByRole('button', { name: '+1 day' }));
    const save = within(retention).getByRole('button', { name: 'Save retention' });
    expect(save).toBeDisabled();
    const bypass = within(retention).getByRole('checkbox', {
      name: 'Bypass the governance retention',
    });
    await user.click(bypass);
    expect(save).toBeEnabled();
    await user.click(save);
    await waitFor(() => {
      expect(setRetention).toHaveBeenCalledWith(
        bucket,
        key,
        expect.objectContaining({ mode: 'GOVERNANCE', bypassGovernance: true }),
        undefined,
      );
    });
    const stored = await services.objects.getRetention(bucket, key);
    expect(Date.parse(stored.retainUntil ?? '')).toBeLessThan(Date.now() + 2 * day);
  });

  it('extends a GOVERNANCE retention without the bypass', async () => {
    const services = await servicesWith('GOVERNANCE', 1);
    const setRetention = vi.spyOn(services.objects, 'setRetention');
    const user = userEvent.setup();
    renderWithProviders(
      <ObjectLockPanel
        bucket={bucket}
        objectKey={key}
        perms={permsWithout('s3:BypassGovernanceRetention')}
      />,
      { services },
    );
    const retention = await screen.findByRole('region', { name: 'Retention' });
    await within(retention).findByText(/^GOVERNANCE until/);
    await user.click(within(retention).getByRole('button', { name: '+1 year' }));
    expect(within(retention).queryByRole('checkbox')).not.toBeInTheDocument();
    await user.click(within(retention).getByRole('button', { name: 'Save retention' }));
    await waitFor(() => {
      expect(setRetention).toHaveBeenCalledWith(
        bucket,
        key,
        expect.not.objectContaining({ bypassGovernance: true }),
        undefined,
      );
    });
  });

  it('tells a user without s3:BypassGovernanceRetention why a retention cannot be removed', async () => {
    const services = await servicesWith('GOVERNANCE', 10);
    const user = userEvent.setup();
    renderWithProviders(
      <ObjectLockPanel
        bucket={bucket}
        objectKey={key}
        perms={permsWithout('s3:BypassGovernanceRetention')}
      />,
      { services },
    );
    const retention = await screen.findByRole('region', { name: 'Retention' });
    await within(retention).findByText(/^GOVERNANCE until/);
    await user.click(within(retention).getByRole('textbox', { name: 'Mode' }));
    await user.click(await screen.findByRole('option', { name: 'No retention' }));
    expect(within(retention).getByRole('alert')).toHaveTextContent(
      'Shortening or removing a GOVERNANCE retention needs the permission s3:BypassGovernanceRetention.',
    );
    expect(within(retention).getByRole('button', { name: 'Save retention' })).toBeDisabled();
  });

  it('never shortens a COMPLIANCE retention', async () => {
    const services = await servicesWith('COMPLIANCE', 10);
    const user = userEvent.setup();
    renderWithProviders(
      <ObjectLockPanel bucket={bucket} objectKey={key} perms={permsWithout()} />,
      {
        services,
      },
    );
    const retention = await screen.findByRole('region', { name: 'Retention' });
    await within(retention).findByText(/^COMPLIANCE until/);
    await user.click(within(retention).getByRole('button', { name: '+1 day' }));
    expect(within(retention).getByRole('alert')).toHaveTextContent(
      'A COMPLIANCE retention can only be extended',
    );
    expect(within(retention).queryByRole('checkbox')).not.toBeInTheDocument();
    expect(within(retention).getByRole('button', { name: 'Save retention' })).toBeDisabled();
  });

  it('toggles the legal hold', async () => {
    const services = createMockServices({ initialAccessKey: 'nexoadmin' });
    const user = userEvent.setup();
    renderWithProviders(
      <ObjectLockPanel bucket={bucket} objectKey={key} perms={permsWithout()} />,
      {
        services,
      },
    );
    const hold = await screen.findByRole('region', { name: 'Legal hold' });
    const toggle = await within(hold).findByRole('switch', { name: 'Legal hold active' });
    expect(toggle).not.toBeChecked();
    await user.click(toggle);
    await waitFor(async () => {
      expect(await services.objects.getLegalHold(bucket, key)).toBe('ON');
    });
    await waitFor(() => {
      expect(within(hold).getByRole('switch', { name: 'Legal hold active' })).toBeChecked();
    });
  });
});

describe('VersionDeleteDialog', () => {
  async function versionOf(services: ReturnType<typeof createMockServices>) {
    const { items } = await services.objects.versions(bucket, key);
    const version = items[0];
    if (version === undefined) throw new Error('no version');
    return version;
  }

  it('explains why a version under COMPLIANCE retention cannot be deleted', async () => {
    const services = await servicesWith('COMPLIANCE', 10);
    const remove = vi.spyOn(services.objects, 'remove');
    renderWithProviders(
      <VersionDeleteDialog
        bucket={bucket}
        version={await versionOf(services)}
        perms={permsWithout()}
        onClose={() => {}}
      />,
      { services },
    );
    const dialog = await screen.findByRole('dialog');
    expect(
      await within(dialog).findByText(/protected by a COMPLIANCE retention until/),
    ).toBeInTheDocument();
    expect(within(dialog).getByRole('button', { name: 'Delete permanently' })).toBeDisabled();
    expect(remove).not.toHaveBeenCalled();
  });

  it('deletes a version under GOVERNANCE retention with the bypass', async () => {
    const services = await servicesWith('GOVERNANCE', 10);
    const remove = vi.spyOn(services.objects, 'remove');
    const onClose = vi.fn();
    const user = userEvent.setup();
    const version = await versionOf(services);
    renderWithProviders(
      <VersionDeleteDialog
        bucket={bucket}
        version={version}
        perms={permsWithout()}
        onClose={onClose}
      />,
      { services },
    );
    const dialog = await screen.findByRole('dialog');
    await within(dialog).findByText(/protected by a GOVERNANCE retention until/);
    const button = within(dialog).getByRole('button', { name: 'Delete permanently' });
    expect(button).toBeDisabled();
    await user.click(
      within(dialog).getByRole('checkbox', { name: 'Bypass the governance retention' }),
    );
    await user.click(button);
    await waitFor(() => {
      expect(onClose).toHaveBeenCalled();
    });
    expect(remove).toHaveBeenCalledWith(bucket, {
      objects: [{ key, versionId: version.versionId }],
      bypassGovernance: true,
    });
  });

  it('reports a refusal of the server clearly', async () => {
    const services = createMockServices({ initialAccessKey: 'nexoadmin' });
    const version = await versionOf(services);
    // The legal hold is set behind the dialog's back: the server refuses.
    vi.spyOn(services.objects, 'getLegalHold').mockResolvedValue('OFF');
    await services.objects.setLegalHold(bucket, key, 'ON', version.versionId);
    const user = userEvent.setup();
    renderWithProviders(
      <VersionDeleteDialog
        bucket={bucket}
        version={version}
        perms={permsWithout()}
        onClose={() => {}}
      />,
      { services },
    );
    const dialog = await screen.findByRole('dialog');
    const button = within(dialog).getByRole('button', { name: 'Delete permanently' });
    await waitFor(() => {
      expect(button).toBeEnabled();
    });
    await user.click(button);
    expect(await within(dialog).findByRole('alert')).toHaveTextContent(
      'The version is protected by object lock: it was not deleted.',
    );
  });
});
