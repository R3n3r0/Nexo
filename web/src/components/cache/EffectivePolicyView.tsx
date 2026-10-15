import { Badge, Code, Group, Stack, Table, Text, UnstyledButton } from '@mantine/core';
import { IconChevronDown, IconChevronRight } from '@tabler/icons-react';
import { useState, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';

import { compactDuration, fieldValue, policyFields } from '@/lib/cache';
import type { CacheEffectivePolicy, CacheLevel } from '@/services';

const levelColors: Record<CacheLevel, string> = {
  default: 'gray',
  global: 'blue',
  distribution: 'grape',
  domain: 'teal',
  behavior: 'orange',
};

/** The level a setting comes from, as a badge. */
export function LevelBadge({ level }: { level: CacheLevel }) {
  const { t } = useTranslation();
  return (
    <Badge size="sm" variant="light" color={levelColors[level]} data-level={level}>
      {t(`cache.levels.${level}`)}
    </Badge>
  );
}

export interface EffectivePolicyViewProps {
  effective: CacheEffectivePolicy;
  /** Accessible name of the region. */
  label: string;
}

/**
 * The policy applying to a request and where each setting comes from
 * (docs/11 §11.7, TD-C-04): whether the cache is on, the policy and the
 * level naming it, the lifetimes, the Cache-Control of the clients and,
 * on demand, every setting with its level.
 */
export function EffectivePolicyView({ effective, label }: EffectivePolicyViewProps) {
  const { t } = useTranslation();
  const [open, setOpen] = useState(false);
  const p = effective.policy;
  const rows: { name: string; value: ReactNode; level?: CacheLevel }[] = [
    {
      name: t('cache.effective.cache'),
      value: effective.enabled ? t('cache.effective.enabled') : t('cache.effective.disabled'),
      level: effective.enabledLevel,
    },
    {
      name: t('cache.effective.policy'),
      value: <Code>{effective.policyName}</Code>,
      level: effective.policyLevel,
    },
    {
      name: t('cache.effective.ttl'),
      value: t('cache.effective.ttlValue', {
        default: compactDuration(p.ttl?.default),
        min: compactDuration(p.ttl?.min),
        max: compactDuration(p.ttl?.max),
      }),
      ...(effective.fields['ttl.default'] !== undefined
        ? { level: effective.fields['ttl.default'] }
        : {}),
    },
    {
      name: t('cache.effective.cacheControl'),
      value:
        (effective.cacheControl ?? '') === '' ? (
          <Text size="sm" c="dimmed" span>
            {t('cache.effective.objectCacheControl')}
          </Text>
        ) : (
          <Code>{effective.cacheControl}</Code>
        ),
      ...(effective.fields.browserCacheControl !== undefined
        ? { level: effective.fields.browserCacheControl }
        : {}),
    },
  ];
  if (effective.domain !== undefined && effective.domain !== '') {
    rows.push({ name: t('cache.effective.domain'), value: <Code>{effective.domain}</Code> });
  }
  if (effective.behavior >= 0) {
    rows.push({
      name: t('cache.effective.behavior'),
      value: t('cache.effective.behaviorValue', { index: effective.behavior + 1 }),
    });
  }
  if (effective.domainQuota !== undefined || effective.distributionQuota !== undefined) {
    rows.push({
      name: t('cache.effective.quota'),
      value: [
        effective.domainQuota !== undefined
          ? t('cache.effective.domainQuota', { quota: String(effective.domainQuota) })
          : null,
        effective.distributionQuota !== undefined
          ? t('cache.effective.distributionQuota', { quota: String(effective.distributionQuota) })
          : null,
      ]
        .filter((x) => x !== null)
        .join(' · '),
    });
  }
  rows.push({
    name: t('cache.effective.autoInvalidate'),
    value: effective.autoInvalidate ? t('cache.effective.on') : t('cache.effective.off'),
  });

  return (
    <Stack gap="xs" component="section" aria-label={label}>
      <Table verticalSpacing={4} aria-label={label}>
        <Table.Tbody>
          {rows.map((r) => (
            <Table.Tr key={r.name}>
              <Table.Th scope="row" w="35%" fw={500}>
                {r.name}
              </Table.Th>
              <Table.Td>
                <Group gap="xs" wrap="wrap">
                  {typeof r.value === 'string' ? <Text size="sm">{r.value}</Text> : r.value}
                  {r.level !== undefined ? <LevelBadge level={r.level} /> : null}
                </Group>
              </Table.Td>
            </Table.Tr>
          ))}
        </Table.Tbody>
      </Table>
      <UnstyledButton
        onClick={() => {
          setOpen((o) => !o);
        }}
        aria-expanded={open}
      >
        <Group gap={4}>
          {open ? <IconChevronDown size={14} /> : <IconChevronRight size={14} />}
          <Text size="sm" c="blue">
            {t('cache.effective.allSettings')}
          </Text>
        </Group>
      </UnstyledButton>
      {open ? (
        <Table verticalSpacing={2} aria-label={t('cache.effective.settingsTable')}>
          <Table.Thead>
            <Table.Tr>
              <Table.Th>{t('cache.effective.setting')}</Table.Th>
              <Table.Th>{t('cache.effective.value')}</Table.Th>
              <Table.Th>{t('cache.effective.level')}</Table.Th>
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {policyFields.map((f) => {
              const level = effective.fields[f];
              return (
                <Table.Tr key={f}>
                  <Table.Td>
                    <Text size="xs" ff="monospace">
                      {f}
                    </Text>
                  </Table.Td>
                  <Table.Td>
                    <Text size="xs" ff="monospace">
                      {fieldValue(p, f)}
                    </Text>
                  </Table.Td>
                  <Table.Td>{level !== undefined ? <LevelBadge level={level} /> : null}</Table.Td>
                </Table.Tr>
              );
            })}
          </Table.Tbody>
        </Table>
      ) : null}
    </Stack>
  );
}
