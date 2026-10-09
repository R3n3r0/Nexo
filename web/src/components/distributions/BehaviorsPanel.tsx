import {
  ActionIcon,
  Badge,
  Button,
  Group,
  Paper,
  Stack,
  Table,
  Text,
  Title,
  Tooltip,
  VisuallyHidden,
} from '@mantine/core';
import { IconArrowDown, IconArrowUp, IconEdit, IconPlus, IconTrash } from '@tabler/icons-react';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';

import { effectiveDefault } from '@/lib/distributions';
import type { Distribution, DistributionBehavior } from '@/services';

import { BehaviorEditor } from './BehaviorEditor';
import { UrlTester } from './UrlTester';

export interface BehaviorsPanelProps {
  draft: Distribution;
  onChange: (next: Distribution) => void;
  /** The draft has unsaved changes. */
  dirty: boolean;
  keyGroups: readonly string[];
  readOnly: boolean;
}

/** Editing state: the index of a behavior, -1 for a new one, 'default'. */
type Editing = number | 'default' | null;

/**
 * D-DST-03: the ordered behaviors of a distribution (the first match
 * wins), the default behavior, and the URL tester highlighting the
 * behavior that serves a URL.
 */
export function BehaviorsPanel({
  draft,
  onChange,
  dirty,
  keyGroups,
  readOnly,
}: BehaviorsPanelProps) {
  const { t } = useTranslation();
  const [editing, setEditing] = useState<Editing>(null);
  const [tested, setTested] = useState<number | null>(null);
  const behaviors = draft.behaviors ?? [];
  const def = effectiveDefault(draft);
  const website = (draft.mode ?? 'website') === 'website';

  const setBehaviors = (next: DistributionBehavior[]) => {
    onChange({ ...draft, behaviors: next });
  };
  const move = (from: number, to: number) => {
    const next = [...behaviors];
    const [b] = next.splice(from, 1);
    if (b === undefined) return;
    next.splice(to, 0, b);
    setBehaviors(next);
  };

  const row = (b: DistributionBehavior, index: number | 'default') => {
    const label =
      index === 'default'
        ? t('distributions.behaviors.default')
        : t('distributions.behaviors.numbered', { n: index + 1 });
    const highlighted = tested !== null && (index === 'default' ? tested === -1 : tested === index);
    return (
      <Table.Tr
        key={index === 'default' ? 'default' : `${String(index)}-${b.pathPattern?.pattern ?? ''}`}
        data-behavior={index === 'default' ? 'default' : String(index)}
        data-tested={highlighted ? 'true' : undefined}
        // A bar on the left marks the tested behavior (a tinted row would
        // lower the contrast of its badges).
        style={highlighted ? { boxShadow: 'inset 4px 0 0 var(--mantine-color-blue-6)' } : undefined}
      >
        <Table.Td>
          {index === 'default' ? (
            <Text size="sm" fs="italic">
              {t('distributions.behaviors.allPaths')}
            </Text>
          ) : (
            <Group gap={4} wrap="nowrap">
              <Badge size="xs" variant="outline" color="gray">
                {t(`pattern.types.${b.pathPattern?.type ?? 'glob'}`)}
              </Badge>
              <Text size="sm" ff="monospace" style={{ overflowWrap: 'anywhere' }}>
                {b.pathPattern?.pattern ?? ''}
              </Text>
            </Group>
          )}
        </Table.Td>
        <Table.Td>
          {b.access !== undefined ? (
            <Badge
              color={b.access === 'deny' ? 'red' : b.access === 'public' ? 'teal' : 'blue'}
              variant="light"
            >
              {t(`distributions.access.${b.access}`)}
              {b.access === 'signed' && b.signing !== undefined ? ` · ${b.signing.keyGroup}` : ''}
            </Badge>
          ) : (
            <Text size="xs" c="dimmed">
              {t('distributions.behaviors.inherited')}
            </Text>
          )}
        </Table.Td>
        <Table.Td>
          <Text size="xs">
            {b.allowedMethods?.join(', ') ?? t('distributions.behaviors.inherited')}
          </Text>
        </Table.Td>
        <Table.Td>
          <Text size="xs" ff="monospace">
            {b.cache?.cacheControl ?? '—'}
          </Text>
        </Table.Td>
        <Table.Td>
          <Group gap={2} wrap="nowrap" justify="flex-end">
            {index !== 'default' ? (
              <>
                <ActionIcon
                  size="sm"
                  variant="subtle"
                  disabled={readOnly || index === 0}
                  aria-label={t('distributions.behaviors.moveUp', { name: label })}
                  onClick={() => {
                    move(index, index - 1);
                  }}
                >
                  <IconArrowUp size={14} />
                </ActionIcon>
                <ActionIcon
                  size="sm"
                  variant="subtle"
                  disabled={readOnly || index === behaviors.length - 1}
                  aria-label={t('distributions.behaviors.moveDown', { name: label })}
                  onClick={() => {
                    move(index, index + 1);
                  }}
                >
                  <IconArrowDown size={14} />
                </ActionIcon>
              </>
            ) : null}
            <Tooltip label={t('distributions.behaviors.edit', { name: label })}>
              <ActionIcon
                variant="subtle"
                aria-label={t('distributions.behaviors.edit', { name: label })}
                onClick={() => {
                  setEditing(index);
                }}
              >
                <IconEdit size={16} />
              </ActionIcon>
            </Tooltip>
            {index !== 'default' && !readOnly ? (
              <Tooltip label={t('distributions.behaviors.remove', { name: label })}>
                <ActionIcon
                  variant="subtle"
                  color="red"
                  aria-label={t('distributions.behaviors.remove', { name: label })}
                  onClick={() => {
                    setBehaviors(behaviors.filter((_, i) => i !== index));
                  }}
                >
                  <IconTrash size={16} />
                </ActionIcon>
              </Tooltip>
            ) : null}
          </Group>
        </Table.Td>
      </Table.Tr>
    );
  };

  const edited: DistributionBehavior | null =
    editing === null
      ? null
      : editing === 'default'
        ? (draft.defaultBehavior ?? def)
        : editing < 0
          ? { pathPattern: { type: 'glob', pattern: '' } }
          : (behaviors[editing] ?? null);

  return (
    <Stack gap="md">
      <Paper
        withBorder
        radius="md"
        p="md"
        component="section"
        aria-label={t('distributions.behaviors.title')}
      >
        <Stack gap="sm">
          <Group justify="space-between" wrap="wrap">
            <Stack gap={2}>
              <Title order={4}>{t('distributions.behaviors.title')}</Title>
              <Text size="sm" c="dimmed">
                {t('distributions.behaviors.description')}
              </Text>
            </Stack>
            {!readOnly ? (
              <Button
                leftSection={<IconPlus size={16} />}
                onClick={() => {
                  setEditing(-1);
                }}
              >
                {t('distributions.behaviors.add')}
              </Button>
            ) : null}
          </Group>
          <Table.ScrollContainer minWidth={640}>
            <Table aria-label={t('distributions.behaviors.tableLabel')} verticalSpacing="xs">
              <Table.Thead>
                <Table.Tr>
                  <Table.Th>{t('distributions.behaviors.paths')}</Table.Th>
                  <Table.Th>{t('distributions.fields.access')}</Table.Th>
                  <Table.Th>{t('distributions.fields.methods')}</Table.Th>
                  <Table.Th>{t('distributions.fields.cacheControl')}</Table.Th>
                  <Table.Th w={140}>
                    <VisuallyHidden>{t('common.actions')}</VisuallyHidden>
                  </Table.Th>
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {behaviors.map((b, i) => row(b, i))}
                {row(draft.defaultBehavior ?? def, 'default')}
              </Table.Tbody>
            </Table>
          </Table.ScrollContainer>
        </Stack>
      </Paper>
      <UrlTester distribution={draft} dirty={dirty} onResult={setTested} />
      {editing !== null && edited !== null ? (
        <BehaviorEditor
          behavior={edited}
          isDefault={editing === 'default'}
          isNew={typeof editing === 'number' && editing < 0}
          keyGroups={keyGroups}
          website={website}
          readOnly={readOnly}
          onClose={() => {
            setEditing(null);
          }}
          onSave={(b) => {
            if (editing === 'default') onChange({ ...draft, defaultBehavior: b });
            else if (editing < 0) setBehaviors([...behaviors, b]);
            else setBehaviors(behaviors.map((x, i) => (i === editing ? b : x)));
            setEditing(null);
          }}
        />
      ) : null}
    </Stack>
  );
}
