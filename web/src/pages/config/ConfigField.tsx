import { NumberInput, PasswordInput, Select, Switch, TextInput } from '@mantine/core';
import { useTranslation } from 'react-i18next';

import { isConfigTrue, sensitiveMask } from '@/lib/config';
import type { ConfigKey } from '@/services';

/** One key of a configuration subsystem, with the input of its type. */
export function ConfigField({
  configKey,
  value,
  error,
  onChange,
}: {
  configKey: ConfigKey;
  value: string;
  error: string | undefined;
  onChange: (value: string) => void;
}) {
  const { t } = useTranslation();
  const label = configKey.name;
  const description = [
    configKey.applied ? '' : t('configuration.notAppliedHint'),
    configKey.description,
    configKey.default !== undefined && configKey.default !== ''
      ? t('configuration.default', { value: configKey.default })
      : '',
  ]
    .filter((s) => s !== '')
    .join(' · ');

  switch (configKey.type) {
    case 'bool':
      return (
        <Switch
          label={label}
          description={description}
          checked={value === '' ? isConfigTrue(configKey.default) : isConfigTrue(value)}
          onChange={(event) => {
            onChange(event.currentTarget.checked ? 'on' : 'off');
          }}
        />
      );
    case 'enum':
      return (
        <Select
          label={label}
          description={description}
          data={configKey.values ?? []}
          value={value === '' ? null : value}
          placeholder={configKey.default}
          onChange={(next) => {
            onChange(next ?? '');
          }}
          clearable
          error={error}
        />
      );
    case 'int':
      return (
        <NumberInput
          label={label}
          description={description}
          value={value === '' ? '' : Number(value)}
          placeholder={configKey.default}
          min={0}
          allowDecimal={false}
          allowNegative={false}
          onChange={(next) => {
            onChange(next === '' ? '' : String(next));
          }}
          error={error}
        />
      );
    default:
      if (configKey.sensitive) {
        return (
          <PasswordInput
            label={label}
            description={
              value === sensitiveMask
                ? `${description} · ${t('configuration.secretSet')}`
                : description
            }
            value={value}
            autoComplete="off"
            onChange={(event) => {
              onChange(event.currentTarget.value);
            }}
            error={error}
          />
        );
      }
      return (
        <TextInput
          label={label}
          description={description}
          value={value}
          placeholder={configKey.default}
          onChange={(event) => {
            onChange(event.currentTarget.value);
          }}
          error={error}
        />
      );
  }
}
