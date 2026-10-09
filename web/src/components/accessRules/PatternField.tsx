import { Box, Code, Group, Select, Stack, Switch, Text, TextInput } from '@mantine/core';
import { useTranslation } from 'react-i18next';

import { patternIssue } from '@/lib/keyPattern';
import type { KeyPattern, KeyPatternType } from '@/services';

export interface PatternFieldProps {
  /** Accessible name of the pattern input. */
  label: string;
  value: KeyPattern | undefined;
  onChange: (value: KeyPattern | undefined) => void;
  /** Offer "whole bucket" (no pattern): the match of an access rule is optional. */
  allowNone?: boolean;
  /** The problem the server located in the pattern on the last save. */
  serverIssue?: { position?: number | undefined; message: string } | undefined;
  disabled?: boolean;
  /** Placeholder per type. */
  placeholders?: Partial<Record<KeyPatternType, string>>;
}

/** The pattern with a caret under the 1-based position of its problem. */
export function PatternCaret({ pattern, position }: { pattern: string; position: number }) {
  const chars = [...pattern];
  const at = Math.min(Math.max(position, 1), chars.length + 1);
  return (
    <Code block aria-hidden="true" style={{ whiteSpace: 'pre', overflowX: 'auto' }}>
      {`${pattern}\n${' '.repeat(at - 1)}^`}
    </Code>
  );
}

/**
 * The key pattern of a rule or a behavior: its type (prefix, glob, RE2
 * regex), the pattern, case-insensitivity, validated as it is typed. A
 * problem is shown with its position, the one the server reports on
 * save included.
 */
export function PatternField({
  label,
  value,
  onChange,
  allowNone = false,
  serverIssue,
  disabled = false,
  placeholders = {},
}: PatternFieldProps) {
  const { t } = useTranslation();
  const type: KeyPatternType | 'none' = value?.type ?? 'none';
  const issue = patternIssue(value);
  const shown =
    serverIssue !== undefined
      ? { position: serverIssue.position ?? 0, message: serverIssue.message, unsupported: false }
      : issue;
  const errorText =
    shown === undefined || shown.unsupported === true
      ? undefined
      : shown.position > 0
        ? t('pattern.errorAt', { position: shown.position, message: shown.message })
        : shown.message;

  const types = [
    ...(allowNone ? [{ value: 'none', label: t('pattern.types.none') }] : []),
    { value: 'prefix', label: t('pattern.types.prefix') },
    { value: 'glob', label: t('pattern.types.glob') },
    { value: 'regex', label: t('pattern.types.regex') },
  ];

  return (
    <Stack gap={6}>
      <Group align="flex-start" gap="xs" wrap="wrap">
        <Select
          label={t('pattern.type')}
          data={types}
          value={type}
          allowDeselect={false}
          disabled={disabled}
          onChange={(next) => {
            if (next === null) return;
            if (next === 'none') onChange(undefined);
            else onChange({ ...(value ?? { pattern: '' }), type: next as KeyPatternType });
          }}
          w={170}
        />
        {value !== undefined ? (
          <TextInput
            label={label}
            value={value.pattern}
            placeholder={placeholders[value.type]}
            disabled={disabled}
            error={errorText}
            description={t(`pattern.help.${value.type}`)}
            styles={{ input: { fontFamily: 'var(--mantine-font-family-monospace)' } }}
            spellCheck={false}
            autoComplete="off"
            onChange={(event) => {
              onChange({ ...value, pattern: event.currentTarget.value });
            }}
            style={{ flex: '1 1 260px' }}
          />
        ) : (
          <Box style={{ flex: '1 1 260px', alignSelf: 'center' }} pt={22}>
            <Text size="sm" c="dimmed">
              {t('pattern.wholeBucket')}
            </Text>
          </Box>
        )}
      </Group>
      {value !== undefined ? (
        <Switch
          label={t('pattern.caseInsensitive')}
          checked={value.caseInsensitive === true}
          disabled={disabled}
          onChange={(event) => {
            const checked = event.currentTarget.checked;
            const next: KeyPattern = { type: value.type, pattern: value.pattern };
            if (checked) next.caseInsensitive = true;
            onChange(next);
          }}
        />
      ) : null}
      {value !== undefined &&
      shown !== undefined &&
      shown.unsupported !== true &&
      shown.position > 0 ? (
        <PatternCaret pattern={value.pattern} position={shown.position} />
      ) : null}
      {shown?.unsupported === true ? (
        <Text size="xs" c="yellow.8" role="status">
          {t('pattern.unsupported', { message: shown.message })}
        </Text>
      ) : null}
    </Stack>
  );
}
