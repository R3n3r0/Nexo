import {
  Anchor,
  Box,
  Button,
  Checkbox,
  Group,
  Loader,
  Table,
  Text,
  VisuallyHidden,
} from '@mantine/core';
import { IconFile, IconFolder } from '@tabler/icons-react';
import { useVirtualizer } from '@tanstack/react-virtual';
import { useEffect, useRef, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router';

import { formatBytes } from '@/lib/format';

import type { BrowserRow } from './rows';

/** Estimated height of a row; rows are measured once rendered. */
const rowHeight = 44;
/** Load the next page when the last rendered row is this close to the end. */
const prefetchDistance = 20;

export interface ObjectTableProps {
  rows: readonly BrowserRow[];
  selection: ReadonlySet<string>;
  onToggle: (row: BrowserRow) => void;
  onToggleAll: (checked: boolean) => void;
  /** Route of a folder row. */
  folderHref: (row: BrowserRow) => string;
  /** Opens the details of an object row. */
  onOpen: (row: BrowserRow) => void;
  renderActions: (row: BrowserRow) => ReactNode;
  hasNextPage: boolean;
  isFetchingNextPage: boolean;
  fetchNextPage: () => void;
  locale?: string | undefined;
  /** Height of the scroll viewport (CSS). */
  height?: string;
}

/**
 * The listing of the object browser: a semantic table virtualized with
 * @tanstack/react-virtual (only the visible rows are in the DOM, so tens
 * of thousands of rows scroll smoothly) and infinite cursor pagination.
 */
export function ObjectTable({
  rows,
  selection,
  onToggle,
  onToggleAll,
  folderHref,
  onOpen,
  renderActions,
  hasNextPage,
  isFetchingNextPage,
  fetchNextPage,
  locale,
  height = 'max(320px, calc(100dvh - 380px))',
}: ObjectTableProps) {
  const { t } = useTranslation();
  const viewport = useRef<HTMLDivElement>(null);
  const virtualizer = useVirtualizer({
    count: rows.length,
    getScrollElement: () => viewport.current,
    estimateSize: () => rowHeight,
    overscan: 12,
    getItemKey: (index) => rows[index]?.id ?? index,
  });
  const items = virtualizer.getVirtualItems();
  const lastIndex = items.length > 0 ? (items[items.length - 1]?.index ?? -1) : -1;

  useEffect(() => {
    if (hasNextPage && !isFetchingNextPage && lastIndex >= rows.length - prefetchDistance) {
      fetchNextPage();
    }
  }, [lastIndex, rows.length, hasNextPage, isFetchingNextPage, fetchNextPage]);

  const paddingTop = items[0]?.start ?? 0;
  const paddingBottom =
    items.length > 0 ? virtualizer.getTotalSize() - (items[items.length - 1]?.end ?? 0) : 0;
  const selectedCount = rows.filter((r) => selection.has(r.id)).length;
  const allSelected = rows.length > 0 && selectedCount === rows.length;

  return (
    <Box>
      <Box
        ref={viewport}
        style={{ height, overflow: 'auto' }}
        tabIndex={0}
        role="region"
        aria-label={t('objects.listing')}
        data-testid="object-viewport"
      >
        <Table
          stickyHeader
          highlightOnHover
          verticalSpacing={6}
          aria-label={t('objects.listing')}
          aria-rowcount={rows.length + 1}
          style={{ tableLayout: 'fixed' }}
        >
          <Table.Thead>
            <Table.Tr aria-rowindex={1}>
              <Table.Th w={44}>
                <Checkbox
                  aria-label={t('objects.selectAll')}
                  checked={allSelected}
                  indeterminate={selectedCount > 0 && !allSelected}
                  disabled={rows.length === 0}
                  onChange={(event) => {
                    onToggleAll(event.currentTarget.checked);
                  }}
                />
              </Table.Th>
              <Table.Th>{t('common.name')}</Table.Th>
              <Table.Th w={110} visibleFrom="sm" style={{ textAlign: 'right' }}>
                {t('objects.size')}
              </Table.Th>
              <Table.Th w={190} visibleFrom="md">
                {t('objects.modified')}
              </Table.Th>
              <Table.Th w={52}>
                <VisuallyHidden>{t('common.actions')}</VisuallyHidden>
              </Table.Th>
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {paddingTop > 0 ? (
              <tr aria-hidden>
                <td colSpan={5} style={{ height: paddingTop, padding: 0, border: 0 }} />
              </tr>
            ) : null}
            {items.map((item) => {
              const row = rows[item.index];
              if (!row) {
                return null;
              }
              const selected = selection.has(row.id);
              return (
                <Table.Tr
                  key={item.key}
                  data-index={item.index}
                  ref={virtualizer.measureElement}
                  aria-rowindex={item.index + 2}
                  bg={selected ? 'var(--mantine-primary-color-light)' : undefined}
                >
                  <Table.Td>
                    <Checkbox
                      aria-label={t('objects.select', { name: row.name })}
                      checked={selected}
                      onChange={() => {
                        onToggle(row);
                      }}
                    />
                  </Table.Td>
                  <Table.Td>
                    <Group gap={8} wrap="nowrap" style={{ minWidth: 0 }}>
                      {row.kind === 'folder' ? (
                        <IconFolder size={18} aria-hidden style={{ flexShrink: 0 }} />
                      ) : (
                        <IconFile size={18} aria-hidden style={{ flexShrink: 0 }} />
                      )}
                      {row.kind === 'folder' ? (
                        <Anchor
                          component={Link}
                          to={folderHref(row)}
                          size="sm"
                          truncate="end"
                          title={row.key}
                        >
                          {row.name}
                        </Anchor>
                      ) : (
                        <Anchor
                          component="button"
                          type="button"
                          size="sm"
                          truncate="end"
                          title={row.key}
                          c="inherit"
                          style={{ textAlign: 'left' }}
                          onClick={() => {
                            onOpen(row);
                          }}
                        >
                          {row.name}
                        </Anchor>
                      )}
                    </Group>
                  </Table.Td>
                  <Table.Td visibleFrom="sm" style={{ textAlign: 'right', whiteSpace: 'nowrap' }}>
                    {row.kind === 'object' ? formatBytes(row.entry.size, locale) : '—'}
                  </Table.Td>
                  <Table.Td visibleFrom="md" style={{ whiteSpace: 'nowrap' }}>
                    {row.kind === 'object' ? (
                      <time dateTime={row.entry.lastModified}>
                        {new Date(row.entry.lastModified).toLocaleString(locale)}
                      </time>
                    ) : (
                      '—'
                    )}
                  </Table.Td>
                  <Table.Td>{renderActions(row)}</Table.Td>
                </Table.Tr>
              );
            })}
            {paddingBottom > 0 ? (
              <tr aria-hidden>
                <td colSpan={5} style={{ height: paddingBottom, padding: 0, border: 0 }} />
              </tr>
            ) : null}
          </Table.Tbody>
        </Table>
      </Box>
      <Group justify="space-between" gap="xs" px="sm" py={6} wrap="wrap">
        <Text size="xs" c="dimmed" aria-live="polite">
          {selectedCount > 0
            ? t('objects.selectedOf', { selected: selectedCount, total: rows.length })
            : t('objects.loadedCount', { total: rows.length })}
        </Text>
        {isFetchingNextPage ? (
          <Loader size="xs" aria-label={t('common.loading')} />
        ) : hasNextPage ? (
          <Button
            size="compact-xs"
            variant="subtle"
            onClick={() => {
              fetchNextPage();
            }}
          >
            {t('objects.loadMore')}
          </Button>
        ) : null}
      </Group>
    </Box>
  );
}
