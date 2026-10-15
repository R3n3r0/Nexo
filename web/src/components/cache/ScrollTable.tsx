import { Table } from '@mantine/core';
import type { ReactNode } from 'react';

export interface ScrollTableProps {
  /** Below this width the table scrolls horizontally. */
  minWidth: number;
  /** Name of the scrollable area (the name of the table). */
  label: string;
  children: ReactNode;
}

/**
 * A table scrolling horizontally on narrow screens. The scrollable area
 * is focusable, so that keyboard users can scroll it even when the table
 * has no focusable content (axe scrollable-region-focusable).
 */
export function ScrollTable({ minWidth, label, children }: ScrollTableProps) {
  return (
    <Table.ScrollContainer
      type="native"
      minWidth={minWidth}
      tabIndex={0}
      role="group"
      aria-label={label}
    >
      {children}
    </Table.ScrollContainer>
  );
}
