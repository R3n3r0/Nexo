import {
  ActionIcon,
  Alert,
  Badge,
  Button,
  Code,
  Group,
  MultiSelect,
  Paper,
  SegmentedControl,
  Stack,
  TagsInput,
  Text,
  TextInput,
  Title,
  Tooltip,
} from '@mantine/core';
import { IconAlertTriangle, IconPlus, IconTrash } from '@tabler/icons-react';
import { useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';

import {
  actionPresets,
  bucketResources,
  documentFromModel,
  modelFromDocument,
  newStatement,
  type ActionPreset,
  type BuilderModel,
  type BuilderStatement,
  type Effect,
} from '@/lib/policyBuilder';
import { adminActions, s3Actions } from '@/lib/policyCatalog';

export interface PolicyBuilderProps {
  /** The parsed JSON document; undefined when the JSON text is not valid. */
  document: unknown;
  /** Called with the rebuilt document after every change. */
  onChange: (document: Record<string, unknown>) => void;
  readOnly?: boolean;
}

const actionData = [
  { group: '*', items: ['*', 's3:*', 'admin:*'] },
  { group: 'S3', items: [...s3Actions] },
  { group: 'Admin', items: [...adminActions] },
];

/**
 * Visual builder of the statements of a policy: effect, actions from the
 * catalog and resources by bucket and prefix. It renders a projection of
 * the JSON document and emits a new document on every change, so the JSON
 * editor and the builder stay synchronised (TD-U-07).
 */
export function PolicyBuilder({ document, onChange, readOnly = false }: PolicyBuilderProps) {
  const { t } = useTranslation();
  const model = useMemo(() => modelFromDocument(document), [document]);

  if (model === undefined) {
    return (
      <Alert color="yellow" icon={<IconAlertTriangle size={18} />}>
        {t('policyBuilder.invalidJson')}
      </Alert>
    );
  }

  const emit = (next: BuilderModel) => {
    onChange(documentFromModel(next));
  };
  const update = (index: number, change: Partial<BuilderStatement>) => {
    emit({
      ...model,
      statements: model.statements.map((s, i) => (i === index ? { ...s, ...change } : s)),
    });
  };

  return (
    <Stack gap="md">
      {model.statements.length === 0 ? (
        <Text c="dimmed" size="sm">
          {t('policyBuilder.empty')}
        </Text>
      ) : null}
      {model.statements.map((statement, index) => (
        <StatementCard
          // Statements have no stable id; the position is what the JSON uses too.
          key={index}
          index={index}
          statement={statement}
          readOnly={readOnly}
          onChange={(change) => {
            update(index, change);
          }}
          onRemove={() => {
            emit({ ...model, statements: model.statements.filter((_, i) => i !== index) });
          }}
        />
      ))}
      {!readOnly ? (
        <Group>
          <Button
            variant="light"
            leftSection={<IconPlus size={16} />}
            onClick={() => {
              emit({ ...model, statements: [...model.statements, newStatement()] });
            }}
          >
            {t('policyBuilder.addStatement')}
          </Button>
        </Group>
      ) : null}
    </Stack>
  );
}

interface StatementCardProps {
  index: number;
  statement: BuilderStatement;
  readOnly: boolean;
  onChange: (change: Partial<BuilderStatement>) => void;
  onRemove: () => void;
}

function StatementCard({ index, statement, readOnly, onChange, onRemove }: StatementCardProps) {
  const { t } = useTranslation();
  const [bucket, setBucket] = useState('');
  const [prefix, setPrefix] = useState('');
  const number = index + 1;
  const sid = typeof statement.original.Sid === 'string' ? statement.original.Sid : '';
  const disabled = readOnly || statement.advanced;
  const hasCondition = statement.original.Condition !== undefined;

  const addBucket = () => {
    const arns = bucketResources(bucket, prefix);
    if (arns.length === 0) return;
    onChange({ resources: [...new Set([...statement.resources, ...arns])] });
    setBucket('');
    setPrefix('');
  };

  const presetLabel: Record<ActionPreset, string> = {
    readOnly: t('policyBuilder.presets.readOnly'),
    readWrite: t('policyBuilder.presets.readWrite'),
    writeOnly: t('policyBuilder.presets.writeOnly'),
    fullS3: t('policyBuilder.presets.fullS3'),
  };

  return (
    <Paper
      withBorder
      p="md"
      radius="md"
      component="section"
      aria-label={t('policyBuilder.statement', { number })}
    >
      <Stack gap="sm">
        <Group justify="space-between" wrap="nowrap">
          <Group gap="xs">
            <Title order={4}>{t('policyBuilder.statement', { number })}</Title>
            {sid !== '' ? <Code>{sid}</Code> : null}
            {hasCondition ? <Badge variant="light">{t('policyBuilder.hasCondition')}</Badge> : null}
          </Group>
          {!readOnly ? (
            <Tooltip label={t('policyBuilder.removeStatement', { number })}>
              <ActionIcon
                variant="subtle"
                color="red"
                aria-label={t('policyBuilder.removeStatement', { number })}
                onClick={onRemove}
              >
                <IconTrash size={16} />
              </ActionIcon>
            </Tooltip>
          ) : null}
        </Group>

        {statement.advanced ? (
          <Alert color="blue" variant="light">
            {t('policyBuilder.advanced')}
          </Alert>
        ) : null}

        <div>
          <Text size="sm" fw={500} id={`effect-${number}`} mb={4}>
            {t('policyBuilder.effect')}
          </Text>
          <SegmentedControl
            aria-labelledby={`effect-${number}`}
            value={statement.effect}
            disabled={disabled}
            onChange={(value) => {
              onChange({ effect: value as Effect });
            }}
            data={[
              { value: 'Allow', label: t('policyBuilder.allow') },
              { value: 'Deny', label: t('policyBuilder.deny') },
            ]}
          />
        </div>

        <MultiSelect
          label={t('policyBuilder.actions')}
          placeholder={t('policyBuilder.actionsPlaceholder')}
          data={actionData}
          value={statement.actions}
          onChange={(actions) => {
            onChange({ actions });
          }}
          searchable
          clearable
          disabled={disabled}
          maxDropdownHeight={280}
          nothingFoundMessage={t('policyBuilder.noAction')}
        />
        {!disabled ? (
          <Group gap="xs">
            <Text size="xs" c="dimmed">
              {t('policyBuilder.presets.label')}
            </Text>
            {(Object.keys(actionPresets) as ActionPreset[]).map((preset) => (
              <Button
                key={preset}
                size="compact-xs"
                variant="default"
                onClick={() => {
                  onChange({
                    actions: [...new Set([...statement.actions, ...actionPresets[preset]])],
                  });
                }}
              >
                {presetLabel[preset]}
              </Button>
            ))}
          </Group>
        ) : null}

        <TagsInput
          label={t('policyBuilder.resources')}
          description={t('policyBuilder.resourcesHint')}
          placeholder="arn:aws:s3:::bucket/prefix/*"
          value={statement.resources}
          onChange={(resources) => {
            onChange({ resources });
          }}
          disabled={disabled}
          clearable
        />
        {!disabled ? (
          <Group align="flex-end" gap="xs" wrap="wrap">
            <TextInput
              label={t('policyBuilder.bucket')}
              value={bucket}
              onChange={(event) => {
                setBucket(event.currentTarget.value);
              }}
              size="xs"
            />
            <TextInput
              label={t('policyBuilder.prefix')}
              placeholder="photos/"
              value={prefix}
              onChange={(event) => {
                setPrefix(event.currentTarget.value);
              }}
              size="xs"
            />
            <Button size="xs" variant="light" onClick={addBucket} disabled={bucket.trim() === ''}>
              {t('policyBuilder.addBucket')}
            </Button>
          </Group>
        ) : null}
      </Stack>
    </Paper>
  );
}
