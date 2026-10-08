import { ActionIcon, Button, Group, Stack, Table, Text, TextInput } from '@mantine/core';
import { IconPlus, IconTrash } from '@tabler/icons-react';
import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';

import { PermissionGuard } from '@/components/iam/PermissionGuard';
import type { TagSet } from '@/services';

interface TagRow {
  id: number;
  key: string;
  value: string;
}

/** Editable list of tags (shared by buckets and objects). */
export function TagEditor({
  tags,
  canEdit,
  action,
  saving,
  onSave,
}: {
  tags: TagSet | undefined;
  canEdit: boolean;
  action: string;
  saving: boolean;
  onSave: (tags: TagSet) => void;
}) {
  const { t } = useTranslation();
  const [rows, setRows] = useState<TagRow[]>([]);
  const [nextId, setNextId] = useState(0);

  useEffect(() => {
    const entries = Object.entries(tags ?? {});
    setRows(entries.map(([key, value], id) => ({ id, key, value })));
    setNextId(entries.length);
  }, [tags]);

  const keys = rows.map((r) => r.key.trim());
  const duplicate = keys.some((k, i) => k !== '' && keys.indexOf(k) !== i);
  const emptyKey = rows.some((r) => r.key.trim() === '');

  return (
    <Stack gap="xs">
      {rows.length === 0 ? (
        <Text size="sm" c="dimmed">
          {t('bucketSettings.tags.empty')}
        </Text>
      ) : (
        <Table withRowBorders={false} verticalSpacing={4}>
          <Table.Thead>
            <Table.Tr>
              <Table.Th>{t('bucketSettings.tags.key')}</Table.Th>
              <Table.Th>{t('bucketSettings.tags.value')}</Table.Th>
              <Table.Th w={40} />
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {rows.map((row, index) => (
              <Table.Tr key={row.id}>
                <Table.Td>
                  <TextInput
                    aria-label={t('bucketSettings.tags.keyN', { n: index + 1 })}
                    value={row.key}
                    disabled={!canEdit}
                    maxLength={128}
                    onChange={(event) => {
                      const key = event.currentTarget.value;
                      setRows((rs) => rs.map((r) => (r.id === row.id ? { ...r, key } : r)));
                    }}
                  />
                </Table.Td>
                <Table.Td>
                  <TextInput
                    aria-label={t('bucketSettings.tags.valueN', { n: index + 1 })}
                    value={row.value}
                    disabled={!canEdit}
                    maxLength={256}
                    onChange={(event) => {
                      const value = event.currentTarget.value;
                      setRows((rs) => rs.map((r) => (r.id === row.id ? { ...r, value } : r)));
                    }}
                  />
                </Table.Td>
                <Table.Td>
                  <ActionIcon
                    variant="subtle"
                    color="red"
                    disabled={!canEdit}
                    aria-label={t('bucketSettings.tags.remove', { key: row.key })}
                    onClick={() => {
                      setRows((rs) => rs.filter((r) => r.id !== row.id));
                    }}
                  >
                    <IconTrash size={16} />
                  </ActionIcon>
                </Table.Td>
              </Table.Tr>
            ))}
          </Table.Tbody>
        </Table>
      )}
      {duplicate ? (
        <Text size="xs" c="red" role="alert">
          {t('bucketSettings.tags.duplicate')}
        </Text>
      ) : null}
      <Group gap="xs">
        <Button
          variant="default"
          size="xs"
          leftSection={<IconPlus size={14} />}
          disabled={!canEdit || rows.length >= 50}
          onClick={() => {
            setRows((rs) => [...rs, { id: nextId, key: '', value: '' }]);
            setNextId((n) => n + 1);
          }}
        >
          {t('bucketSettings.tags.add')}
        </Button>
        <PermissionGuard allowed={canEdit} action={action}>
          {(disabled) => (
            <Button
              size="xs"
              disabled={disabled || duplicate || emptyKey}
              loading={saving}
              onClick={() => {
                onSave(Object.fromEntries(rows.map((r) => [r.key.trim(), r.value])));
              }}
            >
              {t('bucketSettings.tags.save')}
            </Button>
          )}
        </PermissionGuard>
      </Group>
    </Stack>
  );
}
