import { Select, TextInput } from '@mantine/core';
import { useTranslation } from 'react-i18next';

import type { Distribution } from '@/services';

export interface DistributionFieldProps {
  label: string;
  value: string;
  onChange: (id: string) => void;
  /** The distributions to choose from; undefined: the id is typed. */
  choices: readonly Distribution[] | undefined;
  description?: string;
  error?: string | undefined;
  required?: boolean;
}

/** The distribution an operation applies to: chosen in a list, or typed. */
export function DistributionField({
  label,
  value,
  onChange,
  choices,
  description,
  error,
  required = false,
}: DistributionFieldProps) {
  const { t } = useTranslation();
  if (choices === undefined) {
    return (
      <TextInput
        label={label}
        description={description ?? t('cache.distributionTyped')}
        value={value}
        error={error}
        required={required}
        onChange={(event) => {
          onChange(event.currentTarget.value.trim());
        }}
      />
    );
  }
  return (
    <Select
      label={label}
      description={description}
      searchable
      allowDeselect={false}
      required={required}
      error={error}
      nothingFoundMessage={t('cache.noDistribution')}
      data={choices.map((d) => ({ value: d.id, label: d.id }))}
      value={value === '' ? null : value}
      onChange={(next) => {
        onChange(next ?? '');
      }}
    />
  );
}
