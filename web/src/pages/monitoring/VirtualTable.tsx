import { Box, Center, Table, Text } from '@mantine/core';
import { useVirtualizer } from '@tanstack/react-virtual';
import { useRef, type ReactNode } from 'react';

export interface VirtualColumn {
  key: string;
  header: ReactNode;
  width?: number;
}

export interface VirtualTableProps<T> {
  /** The accessible name of the table and of its scrollable area. */
  label: string;
  rows: readonly T[];
  columns: readonly VirtualColumn[];
  rowKey: (row: T) => string | number;
  /** The cells of a row (Table.Td elements, one per column). */
  renderRow: (row: T) => ReactNode;
  /** Shown instead of the rows when there is none. */
  empty: ReactNode;
  /** Estimated height of a row (rows are measured once rendered). */
  estimate?: number;
  height?: string;
  /** Below this width the table scrolls horizontally. */
  minWidth?: number;
}

/**
 * A table of many rows (the live trace and logs keep up to 5000) rendering
 * only the visible ones, its header sticky. The scrollable area is
 * focusable so that keyboard users can scroll it (axe
 * scrollable-region-focusable).
 */
export function VirtualTable<T>({
  label,
  rows,
  columns,
  rowKey,
  renderRow,
  empty,
  estimate = 36,
  height = 'max(320px, calc(100dvh - 460px))',
  minWidth = 960,
}: VirtualTableProps<T>) {
  const viewport = useRef<HTMLDivElement>(null);
  const virtualizer = useVirtualizer({
    count: rows.length,
    getScrollElement: () => viewport.current,
    estimateSize: () => estimate,
    overscan: 10,
    getItemKey: (index) => {
      const row = rows[index];
      return row === undefined ? index : rowKey(row);
    },
  });
  const items = virtualizer.getVirtualItems();
  const paddingTop = items[0]?.start ?? 0;
  const paddingBottom =
    items.length > 0 ? virtualizer.getTotalSize() - (items[items.length - 1]?.end ?? 0) : 0;
  return (
    <Box
      ref={viewport}
      style={{ height, overflow: 'auto' }}
      tabIndex={0}
      role="region"
      aria-label={label}
    >
      <Table
        stickyHeader
        highlightOnHover
        verticalSpacing={4}
        aria-label={label}
        aria-rowcount={rows.length + 1}
        style={{ tableLayout: 'fixed', minWidth }}
      >
        <Table.Thead>
          <Table.Tr aria-rowindex={1}>
            {columns.map((c) => (
              <Table.Th key={c.key} w={c.width}>
                {c.header}
              </Table.Th>
            ))}
          </Table.Tr>
        </Table.Thead>
        <Table.Tbody>
          {rows.length === 0 ? (
            <Table.Tr>
              <Table.Td colSpan={columns.length}>
                <Center py="xl">
                  <Text size="sm" c="dimmed">
                    {empty}
                  </Text>
                </Center>
              </Table.Td>
            </Table.Tr>
          ) : null}
          {paddingTop > 0 ? (
            <tr aria-hidden>
              <td colSpan={columns.length} style={{ height: paddingTop, padding: 0, border: 0 }} />
            </tr>
          ) : null}
          {items.map((item) => {
            const row = rows[item.index];
            if (row === undefined) return null;
            return (
              <Table.Tr
                key={item.key}
                data-index={item.index}
                ref={virtualizer.measureElement}
                aria-rowindex={item.index + 2}
              >
                {renderRow(row)}
              </Table.Tr>
            );
          })}
          {paddingBottom > 0 ? (
            <tr aria-hidden>
              <td
                colSpan={columns.length}
                style={{ height: paddingBottom, padding: 0, border: 0 }}
              />
            </tr>
          ) : null}
        </Table.Tbody>
      </Table>
    </Box>
  );
}
