import { ActionIcon, Badge, Group, Table, Text, Tooltip, VisuallyHidden } from '@mantine/core';
import {
  IconArrowDown,
  IconArrowUp,
  IconEdit,
  IconEye,
  IconGripVertical,
  IconTrash,
} from '@tabler/icons-react';
import { useState, type DragEvent } from 'react';
import { useTranslation } from 'react-i18next';

import { conditionKeys } from '@/lib/accessRules';
import type { BucketAccessRule } from '@/services';

export interface RulesTableProps {
  /** The rules in the order of evaluation of their priorities. */
  rules: readonly BucketAccessRule[];
  canEdit: boolean;
  /** A save is in flight: the order cannot change. */
  busy: boolean;
  /** Rules to highlight: those the tester matched, and the determinant one. */
  highlight?: { matched: ReadonlySet<string>; determinant?: string | undefined } | undefined;
  onMove: (from: number, to: number) => void;
  onEdit: (index: number) => void;
  onDelete: (index: number) => void;
}

/**
 * D-ACC-01: the rules ordered by priority. The order changes with a drag
 * and drop of the rows, or with the move buttons (keyboard); the caller
 * saves the new priorities atomically.
 */
export function RulesTable({
  rules,
  canEdit,
  busy,
  highlight,
  onMove,
  onEdit,
  onDelete,
}: RulesTableProps) {
  const { t } = useTranslation();
  const [dragged, setDragged] = useState<number | null>(null);
  const [over, setOver] = useState<number | null>(null);
  const movable = canEdit && !busy;

  const dropOn = (index: number) => (event: DragEvent) => {
    event.preventDefault();
    const from = dragged ?? Number(event.dataTransfer.getData('text/plain'));
    setDragged(null);
    setOver(null);
    if (Number.isInteger(from) && from !== index) onMove(from, index);
  };

  return (
    <Table.ScrollContainer minWidth={860}>
      <Table aria-label={t('accessRules.tableLabel')} verticalSpacing="xs" highlightOnHover>
        <Table.Thead>
          <Table.Tr>
            <Table.Th w={84}>
              <VisuallyHidden>{t('accessRules.columns.order')}</VisuallyHidden>
            </Table.Th>
            <Table.Th>{t('accessRules.fields.priority')}</Table.Th>
            <Table.Th>{t('accessRules.fields.id')}</Table.Th>
            <Table.Th>{t('accessRules.fields.effect')}</Table.Th>
            <Table.Th>{t('accessRules.fields.principals')}</Table.Th>
            <Table.Th>{t('accessRules.fields.actions')}</Table.Th>
            <Table.Th>{t('accessRules.columns.keys')}</Table.Th>
            <Table.Th>{t('accessRules.columns.conditions')}</Table.Th>
            <Table.Th w={84}>
              <VisuallyHidden>{t('common.actions')}</VisuallyHidden>
            </Table.Th>
          </Table.Tr>
        </Table.Thead>
        <Table.Tbody>
          {rules.map((rule, index) => {
            const matched = highlight?.matched.has(rule.id) === true;
            const decides = highlight?.determinant === rule.id;
            return (
              <Table.Tr
                key={rule.id}
                data-rule={rule.id}
                draggable={movable}
                onDragStart={(event) => {
                  setDragged(index);
                  event.dataTransfer.effectAllowed = 'move';
                  event.dataTransfer.setData('text/plain', String(index));
                }}
                onDragEnd={() => {
                  setDragged(null);
                  setOver(null);
                }}
                onDragOver={(event) => {
                  if (!movable) return;
                  event.preventDefault();
                  event.dataTransfer.dropEffect = 'move';
                  if (over !== index) setOver(index);
                }}
                onDrop={dropOn(index)}
                data-highlight={decides ? 'decides' : matched ? 'applies' : undefined}
                style={{
                  opacity: dragged === index ? 0.5 : 1,
                  // A bar on the left marks the rules of the tested key (a
                  // tinted row would lower the contrast of its badges).
                  boxShadow:
                    over === index && dragged !== null && dragged !== index
                      ? `inset 0 ${dragged > index ? '2px' : '-2px'} 0 var(--mantine-color-blue-6)`
                      : decides
                        ? 'inset 4px 0 0 var(--mantine-color-blue-6)'
                        : matched
                          ? 'inset 4px 0 0 var(--mantine-color-teal-6)'
                          : undefined,
                  cursor: movable ? 'grab' : undefined,
                }}
              >
                <Table.Td>
                  <Group gap={2} wrap="nowrap">
                    <IconGripVertical
                      size={16}
                      aria-hidden="true"
                      color={movable ? 'var(--mantine-color-dimmed)' : 'transparent'}
                    />
                    <ActionIcon
                      size="sm"
                      variant="subtle"
                      disabled={!movable || index === 0}
                      aria-label={t('accessRules.moveUp', { id: rule.id })}
                      onClick={() => {
                        onMove(index, index - 1);
                      }}
                    >
                      <IconArrowUp size={14} />
                    </ActionIcon>
                    <ActionIcon
                      size="sm"
                      variant="subtle"
                      disabled={!movable || index === rules.length - 1}
                      aria-label={t('accessRules.moveDown', { id: rule.id })}
                      onClick={() => {
                        onMove(index, index + 1);
                      }}
                    >
                      <IconArrowDown size={14} />
                    </ActionIcon>
                  </Group>
                </Table.Td>
                <Table.Td>
                  <Text size="sm" ff="monospace">
                    {rule.priority}
                  </Text>
                </Table.Td>
                <Table.Td>
                  <Group gap={4} wrap="nowrap">
                    <Text size="sm" ff="monospace" style={{ overflowWrap: 'anywhere' }}>
                      {rule.id}
                    </Text>
                    {decides ? (
                      <Badge size="xs" color="blue">
                        {t('accessRules.tester.decides')}
                      </Badge>
                    ) : matched ? (
                      <Badge size="xs" color="teal" variant="light">
                        {t('accessRules.tester.applies')}
                      </Badge>
                    ) : null}
                  </Group>
                </Table.Td>
                <Table.Td>
                  <Badge color={rule.effect === 'deny' ? 'red' : 'teal'} variant="light">
                    {t(`accessRules.effects.${rule.effect}`)}
                  </Badge>
                </Table.Td>
                <Table.Td>
                  <Group gap={4}>
                    {rule.principals.map((p) => (
                      <Badge
                        key={p}
                        size="sm"
                        variant={p === '*' ? 'filled' : 'outline'}
                        color={p === '*' ? 'yellow' : 'gray'}
                        autoContrast
                        tt="none"
                      >
                        {p === '*' ? t('accessRules.everyone') : p}
                      </Badge>
                    ))}
                  </Group>
                </Table.Td>
                <Table.Td>
                  <Text size="sm">{rule.actions.join(', ')}</Text>
                </Table.Td>
                <Table.Td>
                  {rule.match === undefined ? (
                    <Text size="sm" fs="italic">
                      {t('pattern.wholeBucket')}
                    </Text>
                  ) : (
                    <Group gap={4} wrap="nowrap">
                      <Badge size="xs" variant="outline" color="gray">
                        {t(`pattern.types.${rule.match.type}`)}
                      </Badge>
                      <Text size="sm" ff="monospace" style={{ overflowWrap: 'anywhere' }}>
                        {rule.match.pattern === '' ? '""' : rule.match.pattern}
                        {rule.match.caseInsensitive === true ? ' (i)' : ''}
                      </Text>
                    </Group>
                  )}
                </Table.Td>
                <Table.Td>
                  <Text size="xs" c="dimmed">
                    {conditionKeys(rule.conditions)
                      .map((k) => t(`accessRules.conditions.${k}`))
                      .join(', ') || '—'}
                  </Text>
                </Table.Td>
                <Table.Td>
                  <Group gap={2} wrap="nowrap" justify="flex-end">
                    <Tooltip
                      label={
                        canEdit
                          ? t('accessRules.edit', { id: rule.id })
                          : t('accessRules.view', { id: rule.id })
                      }
                    >
                      <ActionIcon
                        variant="subtle"
                        aria-label={
                          canEdit
                            ? t('accessRules.edit', { id: rule.id })
                            : t('accessRules.view', { id: rule.id })
                        }
                        onClick={() => {
                          onEdit(index);
                        }}
                      >
                        {canEdit ? <IconEdit size={16} /> : <IconEye size={16} />}
                      </ActionIcon>
                    </Tooltip>
                    {canEdit ? (
                      <Tooltip label={t('accessRules.remove', { id: rule.id })}>
                        <ActionIcon
                          variant="subtle"
                          color="red"
                          disabled={busy}
                          aria-label={t('accessRules.remove', { id: rule.id })}
                          onClick={() => {
                            onDelete(index);
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
          })}
        </Table.Tbody>
      </Table>
    </Table.ScrollContainer>
  );
}
