import {
  Button,
  Checkbox,
  Group,
  NumberInput,
  SimpleGrid,
  Stack,
  Switch,
  Text,
  TextInput,
} from '@mantine/core';
import { useTranslation } from 'react-i18next';

import { ChoiceField } from '@/components/common/ChoiceField';
import {
  formatCacheControl,
  formatDuration,
  parseCacheControl,
  type CacheControlDirectives,
} from '@/lib/cache';

/** Ready-made values of the browser Cache-Control. */
const presets: readonly {
  key: 'immutable' | 'short' | 'revalidate' | 'private';
  value: string;
}[] = [
  { key: 'immutable', value: 'public, max-age=31536000, immutable' },
  { key: 'short', value: 'public, max-age=300' },
  { key: 'revalidate', value: 'public, no-cache' },
  { key: 'private', value: 'private, no-store' },
];

type SecondsDirective = 'maxAge' | 'sMaxAge' | 'staleWhileRevalidate' | 'staleIfError';
type FlagDirective = 'immutable' | 'noCache' | 'noStore' | 'mustRevalidate';

export interface CacheControlBuilderProps {
  /** The Cache-Control sent to the browsers; "" or undefined keeps the one of the objects. */
  value: string | undefined;
  onChange: (value: string | undefined) => void;
  /** An empty value is a setting ("" keeps the objects' header) rather than inherited. */
  emptyIsSet?: boolean;
  error?: string | undefined;
  disabled?: boolean;
}

/**
 * The Cache-Control the clients receive (browserCacheControl): built from
 * its directives or typed as is, with presets; off, the clients receive
 * the Cache-Control stored with the objects.
 */
export function CacheControlBuilder({
  value,
  onChange,
  emptyIsSet = true,
  error,
  disabled = false,
}: CacheControlBuilderProps) {
  const { t } = useTranslation();
  const current = value ?? '';
  const on = current !== '';
  const d = parseCacheControl(current);
  const update = (next: CacheControlDirectives) => {
    const v = formatCacheControl(next);
    onChange(v === '' ? (emptyIsSet ? '' : undefined) : v);
  };
  const seconds = (key: SecondsDirective) => (
    <NumberInput
      key={key}
      label={t(`cache.cacheControl.${key}`)}
      description={
        d[key] !== undefined && d[key] > 0 ? formatDuration(d[key]) : t('cache.cacheControl.unset')
      }
      min={0}
      allowDecimal={false}
      disabled={disabled}
      value={d[key] ?? ''}
      onChange={(n) => {
        update({ ...d, [key]: typeof n === 'number' ? n : undefined });
      }}
    />
  );
  const flag = (key: FlagDirective) => (
    <Checkbox
      key={key}
      label={t(`cache.cacheControl.${key}`)}
      checked={d[key]}
      disabled={disabled}
      onChange={(event) => {
        update({ ...d, [key]: event.currentTarget.checked });
      }}
    />
  );
  return (
    <Stack gap="sm">
      <Switch
        label={t('cache.cacheControl.send')}
        description={on ? t('cache.cacheControl.sendHint') : t('cache.cacheControl.objectHint')}
        checked={on}
        disabled={disabled}
        onChange={(event) => {
          if (event.currentTarget.checked) onChange('public, max-age=3600');
          else onChange(emptyIsSet ? '' : undefined);
        }}
      />
      {on ? (
        <>
          <Group gap="xs" wrap="wrap" aria-label={t('cache.cacheControl.presets')} role="group">
            {presets.map((p) => (
              <Button
                key={p.key}
                size="compact-sm"
                variant={current === p.value ? 'light' : 'default'}
                disabled={disabled}
                onClick={() => {
                  onChange(p.value);
                }}
              >
                {t(`cache.cacheControl.preset.${p.key}`)}
              </Button>
            ))}
          </Group>
          <ChoiceField<CacheControlDirectives['visibility']>
            label={t('cache.cacheControl.visibility')}
            value={d.visibility}
            disabled={disabled}
            choices={[
              { value: 'public', label: 'public' },
              { value: 'private', label: 'private' },
              { value: 'none', label: t('cache.cacheControl.none') },
            ]}
            onChange={(visibility) => {
              update({ ...d, visibility });
            }}
          />
          <SimpleGrid cols={{ base: 1, sm: 2, lg: 4 }} spacing="sm">
            {(['maxAge', 'sMaxAge', 'staleWhileRevalidate', 'staleIfError'] as const).map(seconds)}
          </SimpleGrid>
          <Group gap="md" wrap="wrap">
            {(['immutable', 'noCache', 'noStore', 'mustRevalidate'] as const).map(flag)}
          </Group>
          <TextInput
            label={t('cache.cacheControl.value')}
            description={t('cache.cacheControl.valueHint')}
            ff="monospace"
            value={current}
            error={error}
            disabled={disabled}
            onChange={(event) => {
              onChange(event.currentTarget.value);
            }}
          />
        </>
      ) : (
        <Text size="xs" c="dimmed">
          {t('cache.cacheControl.offHint')}
        </Text>
      )}
    </Stack>
  );
}
