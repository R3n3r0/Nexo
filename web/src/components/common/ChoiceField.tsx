import { SegmentedControl, Stack, Text } from '@mantine/core';
import { useId } from 'react';

export interface Choice<T extends string> {
  value: T;
  label: string;
  /** Shown under the control while the choice is selected. */
  description?: string;
  disabled?: boolean;
}

export interface ChoiceFieldProps<T extends string> {
  label: string;
  value: T;
  choices: readonly Choice<T>[];
  onChange: (value: T) => void;
  disabled?: boolean;
  orientation?: 'horizontal' | 'vertical';
}

/**
 * A labelled choice among a few options (a group of radio buttons drawn
 * as a segmented control) with the description of the selected one.
 */
export function ChoiceField<T extends string>({
  label,
  value,
  choices,
  onChange,
  disabled = false,
  orientation = 'horizontal',
}: ChoiceFieldProps<T>) {
  const id = useId();
  const selected = choices.find((c) => c.value === value);
  return (
    <Stack gap={4}>
      <Text size="sm" fw={500} id={`${id}-label`}>
        {label}
      </Text>
      <SegmentedControl
        aria-labelledby={`${id}-label`}
        value={value}
        disabled={disabled}
        orientation={orientation}
        data={choices.map((c) => ({
          value: c.value,
          label: c.label,
          ...(c.disabled === true ? { disabled: true } : {}),
        }))}
        onChange={(next) => {
          const choice = choices.find((c) => c.value === next);
          if (choice !== undefined) onChange(choice.value);
        }}
        style={{ alignSelf: 'flex-start', maxWidth: '100%' }}
      />
      {selected?.description !== undefined ? (
        <Text size="xs" c="dimmed" aria-live="polite">
          {selected.description}
        </Text>
      ) : null}
    </Stack>
  );
}
