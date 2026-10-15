import { Select, TextInput } from '@mantine/core';
import { useTranslation } from 'react-i18next';

import { ChoiceField } from '@/components/common/ChoiceField';
import { parseDuration, parseSize } from '@/lib/cache';
import type { CacheDuration, CacheSize } from '@/services';

/** The text of an optional setting: "" when unset. */
function text(value: string | number | undefined): string {
  return value === undefined ? '' : String(value);
}

export interface DurationFieldProps {
  label: string;
  value: CacheDuration | undefined;
  /** undefined when the field is emptied (the setting is inherited). */
  onChange: (value: string | undefined) => void;
  description?: string;
  placeholder?: string;
  /** A problem reported by the validation (translated). */
  error?: string | undefined;
  disabled?: boolean;
}

/**
 * A duration of the cache configuration ("1h30m", "365d", "90s"): the
 * text as typed, an invalid one flagged at once; empty means inherited.
 */
export function DurationField({
  label,
  value,
  onChange,
  description,
  placeholder,
  error,
  disabled = false,
}: DurationFieldProps) {
  const { t } = useTranslation();
  const raw = text(value);
  const invalid = raw.trim() !== '' && parseDuration(raw) === null;
  return (
    <TextInput
      label={label}
      description={description}
      placeholder={placeholder}
      value={raw}
      disabled={disabled}
      error={invalid ? t('cache.fields.invalidDuration') : error}
      onChange={(event) => {
        const v = event.currentTarget.value;
        onChange(v.trim() === '' ? undefined : v.trim());
      }}
    />
  );
}

export interface SizeFieldProps {
  label: string;
  value: CacheSize | undefined;
  onChange: (value: string | undefined) => void;
  description?: string;
  placeholder?: string;
  error?: string | undefined;
  disabled?: boolean;
}

/** A size of the cache configuration ("20GiB", "512MiB", bytes); empty means inherited. */
export function SizeField({
  label,
  value,
  onChange,
  description,
  placeholder,
  error,
  disabled = false,
}: SizeFieldProps) {
  const { t } = useTranslation();
  const raw = text(value);
  const invalid = raw.trim() !== '' && parseSize(raw) === null;
  return (
    <TextInput
      label={label}
      description={description}
      placeholder={placeholder}
      value={raw}
      disabled={disabled}
      error={invalid ? t('cache.fields.invalidSize') : error}
      onChange={(event) => {
        const v = event.currentTarget.value;
        onChange(v.trim() === '' ? undefined : v.trim());
      }}
    />
  );
}

export type TriState = 'inherit' | 'on' | 'off';

/** The tri-state of an optional boolean setting. */
function triOf(value: boolean | undefined): TriState {
  return value === undefined ? 'inherit' : value ? 'on' : 'off';
}

/** The optional boolean of a tri-state. */
function boolOf(value: TriState): boolean | undefined {
  return value === 'inherit' ? undefined : value === 'on';
}

export interface TriStateFieldProps {
  label: string;
  value: boolean | undefined;
  onChange: (value: boolean | undefined) => void;
  /** Labels of the three choices (default: Inherit, On, Off). */
  labels?: Partial<Record<TriState, string>>;
  /** What the inherited value is, shown under the control. */
  inheritedHint?: string;
  disabled?: boolean;
}

/**
 * An optional boolean of a level of the cache settings: inherited from
 * the more general level, forced on, or forced off.
 */
export function TriStateField({
  label,
  value,
  onChange,
  labels = {},
  inheritedHint,
  disabled = false,
}: TriStateFieldProps) {
  const { t } = useTranslation();
  return (
    <ChoiceField<TriState>
      label={label}
      value={triOf(value)}
      disabled={disabled}
      choices={[
        {
          value: 'inherit',
          label: labels.inherit ?? t('cache.fields.inherit'),
          ...(inheritedHint !== undefined ? { description: inheritedHint } : {}),
        },
        { value: 'on', label: labels.on ?? t('cache.fields.on') },
        { value: 'off', label: labels.off ?? t('cache.fields.off') },
      ]}
      onChange={(next) => {
        onChange(boolOf(next));
      }}
    />
  );
}

export interface PolicySelectProps {
  label: string;
  value: string | undefined;
  /** The names of the policies (built-in and defined). */
  policies: readonly string[];
  onChange: (value: string | undefined) => void;
  /** Offer "inherited" (no policy named at this level). */
  allowInherit?: boolean;
  description?: string;
  disabled?: boolean;
}

/** The cache policy a level starts from, or none (inherited). */
export function PolicySelect({
  label,
  value,
  policies,
  onChange,
  allowInherit = true,
  description,
  disabled = false,
}: PolicySelectProps) {
  const { t } = useTranslation();
  const inherit = '__inherit__';
  const names = [...policies];
  if (value !== undefined && value !== '' && !names.includes(value)) names.push(value);
  return (
    <Select
      label={label}
      description={description}
      disabled={disabled}
      allowDeselect={false}
      searchable
      data={[
        ...(allowInherit ? [{ value: inherit, label: t('cache.fields.inheritPolicy') }] : []),
        ...names.map((n) => ({ value: n, label: n })),
      ]}
      value={value === undefined || value === '' ? (allowInherit ? inherit : null) : value}
      onChange={(next) => {
        onChange(next === null || next === inherit ? undefined : next);
      }}
    />
  );
}
