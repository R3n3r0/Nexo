import { Paper, Skeleton, Stack, Text } from '@mantine/core';
import type { ReactNode } from 'react';

export interface StatTileProps {
  /** Sentence-case label, no trailing colon. */
  label: string;
  /** Headline value; already formatted (compact numbers, units). */
  value: ReactNode;
  /** Secondary line, e.g. "3 of 4" or a commit hash. */
  hint?: ReactNode;
  loading?: boolean;
}

/** A single headline number. Values wear text colours, never accent colours. */
export function StatTile({ label, value, hint, loading = false }: StatTileProps) {
  return (
    <Paper withBorder p="md" radius="md" component="section" aria-label={label}>
      <Stack gap={4}>
        <Text size="sm" c="dimmed">
          {label}
        </Text>
        {loading ? (
          <Skeleton height={32} width="60%" data-testid="stat-skeleton" />
        ) : (
          <Text fz={28} fw={600} lh={1.2}>
            {value}
          </Text>
        )}
        {hint !== undefined && !loading ? (
          <Text size="xs" c="dimmed">
            {hint}
          </Text>
        ) : null}
      </Stack>
    </Paper>
  );
}
