import {
  Checkbox,
  Group,
  Paper,
  SimpleGrid,
  Stack,
  Switch,
  TagsInput,
  Text,
  Title,
} from '@mantine/core';
import type { ReactNode } from 'react';
import { useTranslation } from 'react-i18next';

import { ChoiceField } from '@/components/common/ChoiceField';
import { queryStringOf, textMediaTypes, type CacheIssue, type QueryStringMode } from '@/lib/cache';
import type { CachePolicySpec } from '@/services';

import { CacheControlBuilder } from './CacheControlBuilder';
import { DurationField, SizeField } from './fields';
import { issuesByField } from './helpers';

function Section({ title, children }: { title: string; children: ReactNode }) {
  return (
    <Paper withBorder radius="md" p="md" component="section" aria-label={title}>
      <Stack gap="sm">
        <Title order={4}>{title}</Title>
        {children}
      </Stack>
    </Paper>
  );
}

const algorithms = ['gzip', 'br', 'zstd'] as const;

export interface PolicySpecFieldsProps {
  spec: CachePolicySpec;
  onChange: (spec: CachePolicySpec) => void;
  issues: readonly CacheIssue[];
  disabled?: boolean;
}

/**
 * Every setting of a cache policy (docs/11 §11.2), grouped: lifetime and
 * stale windows, the Cache-Control of the browsers, the cache key, the
 * bypass conditions, compression, sizes and ranges, content types. The
 * problems of the validation are shown on their fields as they appear.
 */
export function PolicySpecFields({
  spec,
  onChange,
  issues,
  disabled = false,
}: PolicySpecFieldsProps) {
  const { t } = useTranslation();
  const errors = issuesByField(t, issues);
  const err = (field: string) => errors.get(field);
  const set = (patch: Partial<CachePolicySpec>) => {
    onChange({ ...spec, ...patch });
  };
  const ttl = spec.ttl ?? {};
  const key = spec.cacheKey ?? {};
  const bypass = spec.bypass ?? {};
  const compression = spec.compression ?? {};
  const range = spec.rangeCaching ?? {};
  const types = spec.contentTypes ?? {};
  const qs = queryStringOf(key.queryString);

  return (
    <Stack gap="md">
      <Section title={t('cache.policy.sections.lifetime')}>
        <Text size="sm" c="dimmed">
          {t('cache.policy.ttlHint')}
        </Text>
        <SimpleGrid cols={{ base: 1, sm: 3 }} spacing="sm">
          <DurationField
            label={t('cache.policy.ttlMin')}
            value={ttl.min}
            disabled={disabled}
            error={err('ttl.min')}
            onChange={(min) => {
              set({ ttl: { ...ttl, min } });
            }}
          />
          <DurationField
            label={t('cache.policy.ttlDefault')}
            value={ttl.default}
            disabled={disabled}
            error={err('ttl.default')}
            onChange={(v) => {
              set({ ttl: { ...ttl, default: v } });
            }}
          />
          <DurationField
            label={t('cache.policy.ttlMax')}
            value={ttl.max}
            disabled={disabled}
            error={err('ttl.max')}
            onChange={(max) => {
              set({ ttl: { ...ttl, max } });
            }}
          />
        </SimpleGrid>
        <Switch
          label={t('cache.policy.respectObjectCacheControl')}
          description={t('cache.policy.respectObjectCacheControlHint')}
          checked={spec.respectObjectCacheControl === true}
          disabled={disabled}
          onChange={(event) => {
            set({ respectObjectCacheControl: event.currentTarget.checked });
          }}
        />
        <SimpleGrid cols={{ base: 1, sm: 3 }} spacing="sm">
          <DurationField
            label={t('cache.policy.staleWhileRevalidate')}
            description={t('cache.policy.staleWhileRevalidateHint')}
            value={spec.staleWhileRevalidate}
            disabled={disabled}
            error={err('staleWhileRevalidate')}
            onChange={(v) => {
              set({ staleWhileRevalidate: v });
            }}
          />
          <DurationField
            label={t('cache.policy.staleIfError')}
            description={t('cache.policy.staleIfErrorHint')}
            value={spec.staleIfError}
            disabled={disabled}
            error={err('staleIfError')}
            onChange={(v) => {
              set({ staleIfError: v });
            }}
          />
          <DurationField
            label={t('cache.policy.negativeCaching')}
            description={t('cache.policy.negativeCachingHint')}
            value={spec.negativeCaching}
            disabled={disabled}
            error={err('negativeCaching')}
            onChange={(v) => {
              set({ negativeCaching: v });
            }}
          />
        </SimpleGrid>
      </Section>

      <Section title={t('cache.policy.sections.browser')}>
        <CacheControlBuilder
          value={spec.browserCacheControl}
          disabled={disabled}
          error={err('browserCacheControl')}
          onChange={(v) => {
            set({ browserCacheControl: v ?? '' });
          }}
        />
      </Section>

      <Section title={t('cache.policy.sections.key')}>
        <ChoiceField<QueryStringMode>
          label={t('cache.policy.queryString')}
          value={qs.mode}
          disabled={disabled}
          choices={(['none', 'all', 'allowlist', 'denylist'] as const).map((mode) => ({
            value: mode,
            label: t(`cache.policy.queryModes.${mode}`),
            description: t(`cache.policy.queryModeHints.${mode}`),
          }))}
          onChange={(mode) => {
            set({
              cacheKey: {
                ...key,
                queryString:
                  mode === 'allowlist' || mode === 'denylist'
                    ? { mode, params: qs.params }
                    : { mode },
              },
            });
          }}
        />
        {qs.mode === 'allowlist' || qs.mode === 'denylist' ? (
          <TagsInput
            label={t('cache.policy.queryParams')}
            value={qs.params}
            disabled={disabled}
            error={err('cacheKey.queryString')}
            onChange={(params) => {
              set({ cacheKey: { ...key, queryString: { mode: qs.mode, params } } });
            }}
          />
        ) : null}
        <SimpleGrid cols={{ base: 1, sm: 2 }} spacing="sm">
          <TagsInput
            label={t('cache.policy.keyHeaders')}
            description={t('cache.policy.keyHeadersHint')}
            value={key.headers ?? []}
            disabled={disabled}
            error={err('cacheKey.headers')}
            onChange={(headers) => {
              set({ cacheKey: { ...key, headers } });
            }}
          />
          <TagsInput
            label={t('cache.policy.keyCookies')}
            value={key.cookies ?? []}
            disabled={disabled}
            error={err('cacheKey.cookies')}
            onChange={(cookies) => {
              set({ cacheKey: { ...key, cookies } });
            }}
          />
        </SimpleGrid>
        <Switch
          label={t('cache.policy.normalize')}
          description={t('cache.policy.normalizeHint')}
          checked={key.normalize === true}
          disabled={disabled}
          onChange={(event) => {
            set({ cacheKey: { ...key, normalize: event.currentTarget.checked } });
          }}
        />
        <Switch
          label={t('cache.policy.includeHost')}
          description={t('cache.policy.includeHostHint')}
          checked={key.includeHost !== false}
          disabled={disabled}
          onChange={(event) => {
            set({ cacheKey: { ...key, includeHost: event.currentTarget.checked } });
          }}
        />
      </Section>

      <Section title={t('cache.policy.sections.bypass')}>
        <Switch
          label={t('cache.policy.bypassAlways')}
          description={t('cache.policy.bypassAlwaysHint')}
          checked={bypass.always === true}
          disabled={disabled}
          onChange={(event) => {
            set({ bypass: { ...bypass, always: event.currentTarget.checked } });
          }}
        />
        <Switch
          label={t('cache.policy.bypassAuthorization')}
          description={t('cache.policy.bypassAuthorizationHint')}
          checked={bypass.authorization !== false}
          disabled={disabled}
          onChange={(event) => {
            set({ bypass: { ...bypass, authorization: event.currentTarget.checked } });
          }}
        />
        <SimpleGrid cols={{ base: 1, sm: 3 }} spacing="sm">
          <TagsInput
            label={t('cache.policy.bypassCookies')}
            value={bypass.cookies ?? []}
            disabled={disabled}
            error={err('bypass.cookies')}
            onChange={(cookies) => {
              set({ bypass: { ...bypass, cookies } });
            }}
          />
          <TagsInput
            label={t('cache.policy.bypassQuery')}
            value={bypass.queryParams ?? []}
            disabled={disabled}
            error={err('bypass.queryParams')}
            onChange={(queryParams) => {
              set({ bypass: { ...bypass, queryParams } });
            }}
          />
          <TagsInput
            label={t('cache.policy.bypassHeaders')}
            value={bypass.headers ?? []}
            disabled={disabled}
            error={err('bypass.headers')}
            onChange={(headers) => {
              set({ bypass: { ...bypass, headers } });
            }}
          />
        </SimpleGrid>
      </Section>

      <Section title={t('cache.policy.sections.compression')}>
        <Switch
          label={t('cache.policy.compressionEnabled')}
          description={t('cache.policy.compressionHint')}
          checked={compression.enabled === true}
          disabled={disabled}
          onChange={(event) => {
            set({ compression: { ...compression, enabled: event.currentTarget.checked } });
          }}
        />
        <Checkbox.Group
          label={t('cache.policy.algorithms')}
          value={compression.algorithms ?? []}
          error={err('compression.algorithms')}
          onChange={(value) => {
            set({
              compression: {
                ...compression,
                algorithms: algorithms.filter((a) => value.includes(a)),
              },
            });
          }}
        >
          <Group gap="md" mt={4}>
            {algorithms.map((a) => (
              <Checkbox key={a} value={a} label={a} disabled={disabled} />
            ))}
          </Group>
        </Checkbox.Group>
        <SimpleGrid cols={{ base: 1, sm: 2 }} spacing="sm">
          <TagsInput
            label={t('cache.policy.compressionTypes')}
            value={compression.contentTypes ?? []}
            disabled={disabled}
            error={err('compression.contentTypes')}
            placeholder={textMediaTypes[0]}
            onChange={(contentTypes) => {
              set({ compression: { ...compression, contentTypes } });
            }}
          />
          <SizeField
            label={t('cache.policy.compressionMinSize')}
            value={compression.minSize}
            disabled={disabled}
            error={err('compression.minSize')}
            onChange={(minSize) => {
              set({ compression: { ...compression, minSize } });
            }}
          />
        </SimpleGrid>
      </Section>

      <Section title={t('cache.policy.sections.sizes')}>
        <SimpleGrid cols={{ base: 1, sm: 2 }} spacing="sm">
          <SizeField
            label={t('cache.policy.minSize')}
            value={spec.minSize}
            disabled={disabled}
            error={err('minSize')}
            onChange={(minSize) => {
              set({ minSize });
            }}
          />
          <SizeField
            label={t('cache.policy.maxObjectSize')}
            value={spec.maxObjectSize}
            disabled={disabled}
            error={err('maxObjectSize')}
            onChange={(maxObjectSize) => {
              set({ maxObjectSize });
            }}
          />
        </SimpleGrid>
        <Switch
          label={t('cache.policy.rangeCaching')}
          description={t('cache.policy.rangeCachingHint')}
          checked={range.enabled === true}
          disabled={disabled}
          onChange={(event) => {
            set({ rangeCaching: { ...range, enabled: event.currentTarget.checked } });
          }}
        />
        <SizeField
          label={t('cache.policy.blockSize')}
          value={range.blockSize}
          disabled={disabled}
          error={err('rangeCaching.blockSize')}
          onChange={(blockSize) => {
            set({ rangeCaching: { ...range, blockSize } });
          }}
        />
        <SimpleGrid cols={{ base: 1, sm: 2 }} spacing="sm">
          <TagsInput
            label={t('cache.policy.typesAllow')}
            description={t('cache.policy.typesAllowHint')}
            value={types.allow ?? []}
            disabled={disabled}
            error={err('contentTypes.allow')}
            onChange={(allow) => {
              set({ contentTypes: { ...types, allow } });
            }}
          />
          <TagsInput
            label={t('cache.policy.typesDeny')}
            value={types.deny ?? []}
            disabled={disabled}
            error={err('contentTypes.deny')}
            onChange={(deny) => {
              set({ contentTypes: { ...types, deny } });
            }}
          />
        </SimpleGrid>
      </Section>
    </Stack>
  );
}
