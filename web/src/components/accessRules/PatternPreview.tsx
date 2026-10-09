import { Badge, Group, List, Stack, Text } from '@mantine/core';
import { IconCheck, IconMinus } from '@tabler/icons-react';
import { useMemo } from 'react';
import { useTranslation } from 'react-i18next';

import { compileKeyPattern, normalizeKey } from '@/lib/keyPattern';
import type { KeyPattern } from '@/services';

export interface PatternPreviewProps {
  pattern: KeyPattern | undefined;
  keys: readonly string[];
  /** Accessible name of the list. */
  label: string;
}

/**
 * Live preview of a pattern on sample keys (D-ACC-02, TD-A-01): every key,
 * normalized like the server, marked as matching or not, with the named
 * groups a regex captures. Without a pattern every key matches (whole
 * bucket).
 */
export function PatternPreview({ pattern, keys, label }: PatternPreviewProps) {
  const { t } = useTranslation();
  const compiled = useMemo(
    () => (pattern === undefined ? null : compileKeyPattern(pattern)),
    [pattern],
  );
  if (keys.length === 0) {
    return (
      <Text size="sm" c="dimmed">
        {t('pattern.preview.noKeys')}
      </Text>
    );
  }
  if (compiled !== null && !compiled.ok) {
    return (
      <Text size="sm" c="dimmed">
        {compiled.issue.unsupported === true
          ? t('pattern.preview.unsupported')
          : t('pattern.preview.invalid')}
      </Text>
    );
  }
  const rows = keys.map((key) => {
    const normalized = normalizeKey(key);
    const m =
      compiled === null ? { matched: true, captures: {} } : compiled.compiled.match(normalized);
    return { key, normalized, ...m };
  });
  const count = rows.filter((r) => r.matched).length;
  return (
    <Stack gap={4}>
      <Text size="xs" c="dimmed" role="status">
        {t('pattern.preview.summary', { count, total: rows.length })}
      </Text>
      <List spacing={4} size="sm" aria-label={label} listStyleType="none">
        {rows.map((row) => (
          <List.Item
            key={row.key}
            data-matched={row.matched ? 'true' : 'false'}
            aria-label={
              row.matched
                ? t('pattern.preview.matches', { key: row.key })
                : t('pattern.preview.noMatch', { key: row.key })
            }
            icon={
              row.matched ? (
                <IconCheck size={16} color="var(--mantine-color-teal-6)" aria-hidden="true" />
              ) : (
                <IconMinus size={16} color="var(--mantine-color-dimmed)" aria-hidden="true" />
              )
            }
          >
            <Group gap={6} wrap="wrap">
              <Text
                size="sm"
                ff="monospace"
                fw={row.matched ? 600 : 400}
                c={row.matched ? undefined : 'dimmed'}
                style={{ overflowWrap: 'anywhere' }}
              >
                {row.key}
              </Text>
              {row.normalized !== row.key ? (
                <Text size="xs" c="dimmed" ff="monospace">
                  → {row.normalized}
                </Text>
              ) : null}
              {Object.entries(row.captures).map(([name, value]) => (
                <Badge key={name} size="xs" variant="outline" tt="none">
                  {name}={value}
                </Badge>
              ))}
            </Group>
          </List.Item>
        ))}
      </List>
    </Stack>
  );
}
