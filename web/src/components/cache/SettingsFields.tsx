import { MultiSelect, SimpleGrid, Stack, Switch, Text } from '@mantine/core';
import { useTranslation } from 'react-i18next';

import { specOf, validateSpec } from '@/lib/cache';
import type { CacheLevel, CacheSettings } from '@/services';

import { CacheControlBuilder } from './CacheControlBuilder';
import { DurationField, PolicySelect, SizeField, TriStateField } from './fields';
import { issuesByField } from './helpers';

export interface SettingsFieldsProps {
  level: Exclude<CacheLevel, 'default'>;
  settings: CacheSettings;
  onChange: (settings: CacheSettings) => void;
  /** The names of the policies. */
  policies: readonly string[];
  /** The purge webhooks of the distribution (domain level). */
  webhooks?: readonly string[];
  /** What the more general levels give, shown with "inherit". */
  inherited?: { enabled?: boolean; policy?: string };
  disabled?: boolean;
}

/**
 * The cache settings of a level (docs/11 §11.1): whether the cache is on,
 * the policy it starts from, the quota and the purge webhooks (where the
 * level has them), and the overrides of the policy (lifetimes, stale
 * windows, Cache-Control of the browsers). An unset field inherits.
 */
export function SettingsFields({
  level,
  settings,
  onChange,
  policies,
  webhooks = [],
  inherited,
  disabled = false,
}: SettingsFieldsProps) {
  const { t } = useTranslation();
  const set = (patch: Partial<CacheSettings>) => {
    onChange({ ...settings, ...patch });
  };
  const errors = issuesByField(t, validateSpec(specOf(settings)));
  const ttl = settings.ttl ?? {};
  const overridesCacheControl = settings.browserCacheControl !== undefined;
  const hasQuota = level === 'distribution' || level === 'domain';
  const hasAuto = level === 'global' || level === 'distribution';
  return (
    <Stack gap="sm">
      <TriStateField
        label={t('cache.settings.enabled')}
        value={settings.enabled}
        disabled={disabled}
        labels={{ on: t('cache.settings.enabledOn'), off: t('cache.settings.enabledOff') }}
        {...(inherited?.enabled !== undefined
          ? {
              inheritedHint: inherited.enabled
                ? t('cache.settings.inheritsOn')
                : t('cache.settings.inheritsOff'),
            }
          : {})}
        onChange={(enabled) => {
          set({ enabled });
        }}
      />
      <SimpleGrid cols={{ base: 1, sm: hasQuota ? 2 : 1 }} spacing="sm">
        <PolicySelect
          label={t('cache.settings.policy')}
          description={
            inherited?.policy !== undefined
              ? t('cache.settings.inheritsPolicy', { policy: inherited.policy })
              : t('cache.settings.policyHint')
          }
          value={settings.policy}
          policies={policies}
          disabled={disabled}
          onChange={(policy) => {
            set({ policy });
          }}
        />
        {hasQuota ? (
          <SizeField
            label={t('cache.settings.quota')}
            description={t(`cache.settings.quotaHint.${level}`)}
            placeholder="20GiB"
            value={settings.quota}
            disabled={disabled}
            onChange={(quota) => {
              set({ quota });
            }}
          />
        ) : null}
      </SimpleGrid>
      {hasAuto ? (
        <TriStateField
          label={t('cache.settings.autoInvalidate')}
          value={settings.autoInvalidate}
          disabled={disabled}
          inheritedHint={t('cache.settings.autoInvalidateHint')}
          onChange={(autoInvalidate) => {
            set({ autoInvalidate });
          }}
        />
      ) : null}
      {level === 'domain' ? (
        <MultiSelect
          label={t('cache.settings.purgeWebhooks')}
          description={
            webhooks.length === 0
              ? t('cache.settings.noWebhooks')
              : t('cache.settings.purgeWebhooksHint')
          }
          data={[...webhooks]}
          value={settings.purgeWebhooks ?? []}
          disabled={disabled || webhooks.length === 0}
          onChange={(purgeWebhooks) => {
            set({ purgeWebhooks: purgeWebhooks.length === 0 ? undefined : purgeWebhooks });
          }}
        />
      ) : null}
      <Text size="sm" fw={500} mt="xs">
        {t('cache.settings.overrides')}
      </Text>
      <Text size="xs" c="dimmed">
        {t('cache.settings.overridesHint')}
      </Text>
      <SimpleGrid cols={{ base: 1, sm: 3 }} spacing="sm">
        <DurationField
          label={t('cache.policy.ttlMin')}
          placeholder={t('cache.settings.inherited')}
          value={ttl.min}
          disabled={disabled}
          error={errors.get('ttl.min')}
          onChange={(min) => {
            set({ ttl: { ...ttl, min } });
          }}
        />
        <DurationField
          label={t('cache.policy.ttlDefault')}
          placeholder={t('cache.settings.inherited')}
          value={ttl.default}
          disabled={disabled}
          error={errors.get('ttl.default')}
          onChange={(v) => {
            set({ ttl: { ...ttl, default: v } });
          }}
        />
        <DurationField
          label={t('cache.policy.ttlMax')}
          placeholder={t('cache.settings.inherited')}
          value={ttl.max}
          disabled={disabled}
          error={errors.get('ttl.max')}
          onChange={(max) => {
            set({ ttl: { ...ttl, max } });
          }}
        />
        <DurationField
          label={t('cache.policy.staleWhileRevalidate')}
          placeholder={t('cache.settings.inherited')}
          value={settings.staleWhileRevalidate}
          disabled={disabled}
          error={errors.get('staleWhileRevalidate')}
          onChange={(staleWhileRevalidate) => {
            set({ staleWhileRevalidate });
          }}
        />
        <DurationField
          label={t('cache.policy.staleIfError')}
          placeholder={t('cache.settings.inherited')}
          value={settings.staleIfError}
          disabled={disabled}
          error={errors.get('staleIfError')}
          onChange={(staleIfError) => {
            set({ staleIfError });
          }}
        />
        <DurationField
          label={t('cache.policy.negativeCaching')}
          placeholder={t('cache.settings.inherited')}
          value={settings.negativeCaching}
          disabled={disabled}
          error={errors.get('negativeCaching')}
          onChange={(negativeCaching) => {
            set({ negativeCaching });
          }}
        />
      </SimpleGrid>
      <TriStateField
        label={t('cache.policy.respectObjectCacheControl')}
        value={settings.respectObjectCacheControl}
        disabled={disabled}
        onChange={(respectObjectCacheControl) => {
          set({ respectObjectCacheControl });
        }}
      />
      <Switch
        label={t('cache.settings.overrideCacheControl')}
        checked={overridesCacheControl}
        disabled={disabled}
        onChange={(event) => {
          set({
            browserCacheControl: event.currentTarget.checked ? 'public, max-age=3600' : undefined,
          });
        }}
      />
      {overridesCacheControl ? (
        <CacheControlBuilder
          value={settings.browserCacheControl}
          disabled={disabled}
          error={errors.get('browserCacheControl')}
          onChange={(browserCacheControl) => {
            set({ browserCacheControl: browserCacheControl ?? '' });
          }}
        />
      ) : null}
    </Stack>
  );
}
