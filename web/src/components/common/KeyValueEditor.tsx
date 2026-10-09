import { ActionIcon, Button, Group, Input, Stack, Text, TextInput } from '@mantine/core';
import { IconPlus, IconTrash } from '@tabler/icons-react';
import { useState, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';

export interface KeyValueEditorProps {
  label: string;
  description?: ReactNode;
  value: Record<string, string>;
  onChange: (value: Record<string, string>) => void;
  keyLabel: string;
  valueLabel: string;
  keyPlaceholder?: string;
  valuePlaceholder?: string;
  disabled?: boolean;
  /** Error of the whole field. */
  error?: ReactNode;
}

interface Row {
  id: number;
  key: string;
  value: string;
}

function rowsOf(value: Record<string, string>, firstId: number): Row[] {
  return Object.entries(value).map(([k, v], i) => ({ id: firstId + i, key: k, value: v }));
}

function mapOf(rows: readonly Row[]): Record<string, string> {
  return Object.fromEntries(
    rows.filter((r) => r.key.trim() !== '').map((r) => [r.key.trim(), r.value]),
  );
}

/**
 * Editor of a string map (headers, captured groups) as rows of two text
 * fields. Rows with an empty name are kept while edited and dropped from
 * the value; duplicated names keep the last row.
 */
export function KeyValueEditor({
  label,
  description,
  value,
  onChange,
  keyLabel,
  valueLabel,
  keyPlaceholder,
  valuePlaceholder,
  disabled = false,
  error,
}: KeyValueEditorProps) {
  const { t } = useTranslation();
  const [rows, setRows] = useState<Row[]>(() => rowsOf(value, 0));
  const [nextId, setNextId] = useState(rows.length);
  const serialized = JSON.stringify(value);
  const [seen, setSeen] = useState(serialized);

  // A value changed by the caller (a preset applied, a reload) replaces the
  // rows; the value the rows produced themselves keeps them (and the rows
  // being typed, without a name yet).
  if (serialized !== seen) {
    setSeen(serialized);
    if (JSON.stringify(mapOf(rows)) !== serialized) {
      setRows(rowsOf(value, nextId));
      setNextId(nextId + Object.keys(value).length);
    }
  }

  const emit = (next: Row[]) => {
    setRows(next);
    onChange(mapOf(next));
  };

  return (
    <Input.Wrapper label={label} description={description} error={error} labelElement="div">
      <Stack gap={6} mt={4} role="group" aria-label={label}>
        {rows.length === 0 ? (
          <Text size="sm" c="dimmed">
            {t('keyValue.empty')}
          </Text>
        ) : null}
        {rows.map((row, index) => (
          <Group key={row.id} gap="xs" wrap="nowrap" align="flex-start">
            <TextInput
              aria-label={`${keyLabel} ${String(index + 1)}`}
              placeholder={keyPlaceholder}
              value={row.key}
              disabled={disabled}
              onChange={(event) => {
                const key = event.currentTarget.value;
                emit(rows.map((r) => (r.id === row.id ? { ...r, key } : r)));
              }}
              style={{ flex: '1 1 40%' }}
            />
            <TextInput
              aria-label={`${valueLabel} ${String(index + 1)}`}
              placeholder={valuePlaceholder}
              value={row.value}
              disabled={disabled}
              onChange={(event) => {
                const v = event.currentTarget.value;
                emit(rows.map((r) => (r.id === row.id ? { ...r, value: v } : r)));
              }}
              style={{ flex: '1 1 60%' }}
            />
            <ActionIcon
              variant="subtle"
              color="red"
              size="lg"
              disabled={disabled}
              aria-label={t('keyValue.remove', {
                name: row.key === '' ? String(index + 1) : row.key,
              })}
              onClick={() => {
                emit(rows.filter((r) => r.id !== row.id));
              }}
            >
              <IconTrash size={16} />
            </ActionIcon>
          </Group>
        ))}
        <Group>
          <Button
            size="xs"
            variant="default"
            leftSection={<IconPlus size={14} />}
            disabled={disabled}
            onClick={() => {
              setRows([...rows, { id: nextId, key: '', value: '' }]);
              setNextId(nextId + 1);
            }}
          >
            {t('keyValue.add', { label })}
          </Button>
        </Group>
      </Stack>
    </Input.Wrapper>
  );
}
