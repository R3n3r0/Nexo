import {
  ActionIcon,
  Autocomplete,
  Badge,
  Button,
  Checkbox,
  CopyButton,
  Group,
  Modal,
  Paper,
  Skeleton,
  Stack,
  Table,
  Tabs,
  Text,
  Tooltip,
  VisuallyHidden,
} from '@mantine/core';
import { IconCheck, IconCopy, IconLinkOff, IconList, IconTrash } from '@tabler/icons-react';
import { useQuery } from '@tanstack/react-query';
import { useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useSearchParams } from 'react-router';

import { useSession } from '@/auth/SessionProvider';
import { usePermissions } from '@/auth/usePermissions';
import { ConfirmDialog, ErrorAlert, PageHeader } from '@/components/iam/common';
import { ShareLinks } from '@/components/share/ShareLinks';
import { UploadGenerator } from '@/components/share/UploadGenerator';
import { iamKeys, useInvalidatingMutation } from '@/lib/iamQueries';
import { useServices, type Share } from '@/services';

type Pending =
  | { kind: 'one'; share: Share }
  | { kind: 'selected'; ids: string[] }
  | { kind: 'all'; owner: string };

/**
 * Shares page (D-SHR-03): active managed links (the caller's own; every
 * user's for administrators), filters by bucket and owner, single,
 * multiple and per-user revocation; and the upload generator (D-SHR-02).
 */
export function SharesPage() {
  const { t } = useTranslation();
  const [params, setParams] = useSearchParams();
  const tab = params.get('tab') === 'upload' ? 'upload' : 'links';
  return (
    <Stack gap="lg">
      <PageHeader title={t('shares.title')} description={t('shares.subtitle')} />
      <Tabs
        value={tab}
        onChange={(value) => {
          const next = new URLSearchParams(params);
          if (value === 'upload') next.set('tab', 'upload');
          else next.delete('tab');
          setParams(next, { replace: true });
        }}
        keepMounted={false}
      >
        <Tabs.List>
          <Tabs.Tab value="links">{t('shares.tabs.links')}</Tabs.Tab>
          <Tabs.Tab value="upload">{t('shares.tabs.upload')}</Tabs.Tab>
        </Tabs.List>
        <Tabs.Panel value="links" pt="md">
          <ActiveLinks />
        </Tabs.Panel>
        <Tabs.Panel value="upload" pt="md">
          <UploadGenerator />
        </Tabs.Panel>
      </Tabs>
    </Stack>
  );
}

function ActiveLinks() {
  const { t, i18n } = useTranslation();
  const { shares, users } = useServices();
  const { session } = useSession();
  const { can } = usePermissions();
  const isAdmin = can('admin:ManageShares');
  const me = session?.accessKey ?? '';
  const [bucket, setBucket] = useState('');
  const [owner, setOwner] = useState('');
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [pending, setPending] = useState<Pending | null>(null);
  // The prefix download share whose per-object links are shown.
  const [linksOf, setLinksOf] = useState<Share | null>(null);

  const filter = {
    ...(bucket.trim() !== '' ? { bucket: bucket.trim() } : {}),
    ...(isAdmin && owner.trim() !== '' ? { owner: owner.trim() } : {}),
  };
  const list = useQuery({
    queryKey: iamKeys.shareList(filter),
    queryFn: () => shares.list(filter),
  });
  const userList = useQuery({
    queryKey: iamKeys.users,
    queryFn: () => users.list(),
    enabled: isAdmin && can('admin:ListUsers'),
  });

  const revoke = useInvalidatingMutation({
    mutationFn: async (action: Pending) => {
      switch (action.kind) {
        case 'one':
          await shares.revoke(action.share.id);
          return 1;
        case 'selected':
          await Promise.all(action.ids.map((id) => shares.revoke(id)));
          return action.ids.length;
        case 'all':
          return shares.revokeAll(action.owner === me ? undefined : action.owner);
      }
    },
    invalidate: [iamKeys.shares],
    onSuccess: () => {
      setPending(null);
      setSelected(new Set());
    },
  });

  const items = list.data;
  const buckets = useMemo(() => [...new Set((items ?? []).map((s) => s.bucket))].sort(), [items]);
  const allSelected =
    items !== undefined && items.length > 0 && items.every((s) => selected.has(s.id));
  const formatDate = (iso: string) =>
    new Date(iso).toLocaleString(i18n.resolvedLanguage, {
      dateStyle: 'medium',
      timeStyle: 'short',
    });
  const revokeAllOwner = isAdmin && owner.trim() !== '' ? owner.trim() : me;

  const confirmMessage = (action: Pending | null): string => {
    if (action === null) return '';
    switch (action.kind) {
      case 'one':
        return t('shares.revoke.one', { name: `${action.share.bucket}/${action.share.key}` });
      case 'selected':
        return t('shares.revoke.selected', { count: action.ids.length });
      case 'all':
        return action.owner === me
          ? t('shares.revoke.allMine')
          : t('shares.revoke.allOf', { owner: action.owner });
    }
  };

  return (
    <Stack gap="md">
      <Group align="flex-end" gap="sm" wrap="wrap">
        <Autocomplete
          label={t('shares.filter.bucket')}
          data={buckets}
          value={bucket}
          onChange={setBucket}
          w={220}
        />
        {isAdmin ? (
          <Autocomplete
            label={t('shares.filter.owner')}
            placeholder={t('shares.filter.everyone')}
            data={(userList.data ?? []).map((u) => u.accessKey).concat(me === '' ? [] : [me])}
            value={owner}
            onChange={setOwner}
            w={220}
          />
        ) : null}
        <Button
          variant="default"
          color="red"
          leftSection={<IconTrash size={16} />}
          disabled={selected.size === 0}
          onClick={() => {
            revoke.reset();
            setPending({ kind: 'selected', ids: [...selected] });
          }}
        >
          {t('shares.revoke.selectedButton', { count: selected.size })}
        </Button>
        <Button
          variant="light"
          color="red"
          leftSection={<IconLinkOff size={16} />}
          onClick={() => {
            revoke.reset();
            setPending({ kind: 'all', owner: revokeAllOwner });
          }}
        >
          {revokeAllOwner === me
            ? t('shares.revoke.allMineButton')
            : t('shares.revoke.allOfButton', { owner: revokeAllOwner })}
        </Button>
      </Group>

      {list.isError ? (
        <ErrorAlert
          error={list.error}
          title={t('shares.loadError')}
          onRetry={() => {
            void list.refetch();
          }}
        />
      ) : null}

      <Paper withBorder radius="md">
        <Table.ScrollContainer minWidth={760}>
          <Table striped highlightOnHover verticalSpacing="sm" aria-label={t('shares.tabs.links')}>
            <Table.Thead>
              <Table.Tr>
                <Table.Th w={40}>
                  <Checkbox
                    aria-label={t('shares.selectAll')}
                    checked={allSelected}
                    indeterminate={selected.size > 0 && !allSelected}
                    onChange={() => {
                      setSelected(
                        allSelected ? new Set() : new Set((items ?? []).map((s) => s.id)),
                      );
                    }}
                  />
                </Table.Th>
                <Table.Th>{t('shares.columns.object')}</Table.Th>
                <Table.Th>{t('shares.columns.permission')}</Table.Th>
                <Table.Th>{t('shares.columns.owner')}</Table.Th>
                <Table.Th>{t('shares.columns.expires')}</Table.Th>
                <Table.Th>
                  <VisuallyHidden>{t('common.actions')}</VisuallyHidden>
                </Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {list.isPending ? (
                <Table.Tr>
                  <Table.Td colSpan={6}>
                    <Skeleton h={20} />
                  </Table.Td>
                </Table.Tr>
              ) : null}
              {items !== undefined && items.length === 0 ? (
                <Table.Tr>
                  <Table.Td colSpan={6}>
                    <Text c="dimmed" ta="center" py="md">
                      {t('shares.empty')}
                    </Text>
                  </Table.Td>
                </Table.Tr>
              ) : null}
              {(items ?? []).map((share) => {
                const expired = Date.parse(share.expiresAt) <= Date.now();
                const name = `${share.bucket}/${share.key}`;
                return (
                  <Table.Tr key={share.id}>
                    <Table.Td>
                      <Checkbox
                        aria-label={t('shares.select', { name })}
                        checked={selected.has(share.id)}
                        onChange={(event) => {
                          const checked = event.currentTarget.checked;
                          setSelected((current) => {
                            const next = new Set(current);
                            if (checked) next.add(share.id);
                            else next.delete(share.id);
                            return next;
                          });
                        }}
                      />
                    </Table.Td>
                    <Table.Td>
                      <Group gap={4} wrap="nowrap">
                        <Text size="sm" ff="monospace" style={{ wordBreak: 'break-all' }}>
                          {name}
                        </Text>
                        {share.prefix ? (
                          <Badge size="xs" variant="outline">
                            {t('shares.prefix')}
                          </Badge>
                        ) : null}
                      </Group>
                      <Text size="xs" c="dimmed">
                        {t('shares.createdAt', { date: formatDate(share.createdAt) })}
                      </Text>
                    </Table.Td>
                    <Table.Td>
                      <Badge
                        variant="light"
                        color={share.permission === 'upload' ? 'grape' : 'blue'}
                      >
                        {t(`shares.permission.${share.permission}`)}
                      </Badge>
                    </Table.Td>
                    <Table.Td>
                      <Text size="sm">{share.owner}</Text>
                    </Table.Td>
                    <Table.Td>
                      <Group gap={4}>
                        <Text size="sm">{formatDate(share.expiresAt)}</Text>
                        {expired ? (
                          <Badge size="xs" color="red" variant="light">
                            {t('shares.expired')}
                          </Badge>
                        ) : null}
                      </Group>
                    </Table.Td>
                    <Table.Td>
                      <Group gap={4} wrap="nowrap">
                        {share.prefix && share.permission === 'download' ? (
                          // The URL of a prefix share only lists it: its
                          // objects are handed out one link each.
                          <Tooltip label={t('shares.linksFor', { name })}>
                            <ActionIcon
                              variant="subtle"
                              aria-label={t('shares.linksFor', { name })}
                              onClick={() => {
                                setLinksOf(share);
                              }}
                            >
                              <IconList size={16} />
                            </ActionIcon>
                          </Tooltip>
                        ) : share.url !== undefined ? (
                          <CopyButton value={share.url}>
                            {({ copied, copy }) => (
                              <Tooltip label={copied ? t('common.copied') : t('shares.copy')}>
                                <ActionIcon
                                  variant="subtle"
                                  aria-label={t('shares.copyFor', { name })}
                                  onClick={copy}
                                >
                                  {copied ? <IconCheck size={16} /> : <IconCopy size={16} />}
                                </ActionIcon>
                              </Tooltip>
                            )}
                          </CopyButton>
                        ) : null}
                        <Tooltip label={t('shares.revoke.action', { name })}>
                          <ActionIcon
                            variant="subtle"
                            color="red"
                            aria-label={t('shares.revoke.action', { name })}
                            onClick={() => {
                              revoke.reset();
                              setPending({ kind: 'one', share });
                            }}
                          >
                            <IconLinkOff size={16} />
                          </ActionIcon>
                        </Tooltip>
                      </Group>
                    </Table.Td>
                  </Table.Tr>
                );
              })}
            </Table.Tbody>
          </Table>
        </Table.ScrollContainer>
      </Paper>

      <Modal
        opened={linksOf !== null}
        onClose={() => {
          setLinksOf(null);
        }}
        title={t('shares.linksTitle', {
          name: linksOf !== null ? `${linksOf.bucket}/${linksOf.key}` : '',
        })}
        size="lg"
      >
        {linksOf !== null ? <ShareLinks shareId={linksOf.id} /> : null}
      </Modal>

      <ConfirmDialog
        opened={pending !== null}
        title={t('shares.revoke.title')}
        message={confirmMessage(pending)}
        confirmLabel={t('shares.revoke.confirm')}
        loading={revoke.isPending}
        error={revoke.isError ? revoke.error : undefined}
        onCancel={() => {
          setPending(null);
        }}
        onConfirm={() => {
          if (pending) revoke.mutate(pending);
        }}
      />
    </Stack>
  );
}
