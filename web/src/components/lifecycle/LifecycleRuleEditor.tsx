import {
  ActionIcon,
  Alert,
  Button,
  Fieldset,
  Group,
  Modal,
  NumberInput,
  SegmentedControl,
  Select,
  Stack,
  Switch,
  Text,
  TextInput,
} from '@mantine/core';
import { IconPlus, IconTrash } from '@tabler/icons-react';
import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';

import { CodeEditor } from '@/components/code/CodeEditor';
import {
  emptyRuleForm,
  formToRule,
  isValidRuleForm,
  ruleFromJson,
  ruleFromXml,
  ruleToForm,
  ruleToJson,
  ruleToXml,
  validateRuleForm,
  type ExpirationKind,
  type LifecycleRuleForm,
  type NumberField,
  type RuleFormIssues,
  type RuleIssue,
} from '@/lib/lifecycle';
import type { LifecycleRule, RuleProblem } from '@/services';

/** How the rule is edited: the guided form or one of its code views. */
export type RuleEditorView = 'form' | 'json' | 'xml';

const sizeUnits = { B: 1, KiB: 1024, MiB: 1024 ** 2, GiB: 1024 ** 3 } as const;
type SizeUnit = keyof typeof sizeUnits;

/** The largest unit expressing bytes as a whole number. */
function unitOf(bytes: NumberField): SizeUnit {
  if (bytes === '' || bytes === 0) return 'MiB';
  for (const unit of ['GiB', 'MiB', 'KiB'] as const) {
    if (bytes % sizeUnits[unit] === 0) return unit;
  }
  return 'B';
}

/** A size bound of the filter: a number and a unit, in bytes for the form. */
function SizeField({
  label,
  value,
  error,
  disabled,
  onChange,
}: {
  label: string;
  value: NumberField;
  error: string | undefined;
  disabled: boolean;
  onChange: (bytes: NumberField) => void;
}) {
  const { t } = useTranslation();
  const [unit, setUnit] = useState<SizeUnit>(() => unitOf(value));
  const shown = value === '' ? '' : value / sizeUnits[unit];
  return (
    <Group align="flex-end" gap="xs" wrap="nowrap">
      <NumberInput
        label={label}
        min={0}
        allowNegative={false}
        decimalScale={unit === 'B' ? 0 : 3}
        value={shown}
        disabled={disabled}
        error={error}
        onChange={(next) => {
          onChange(next === '' ? '' : Math.round(Number(next) * sizeUnits[unit]));
        }}
        style={{ flex: 1 }}
      />
      <Select
        aria-label={t('lifecycle.editor.sizeUnit', { field: label })}
        data={Object.keys(sizeUnits)}
        value={unit}
        allowDeselect={false}
        disabled={disabled}
        onChange={(next) => {
          if (next === null) return;
          const nextUnit = next as SizeUnit;
          setUnit(nextUnit);
          if (value !== '') onChange(Math.round((value / sizeUnits[unit]) * sizeUnits[nextUnit]));
        }}
        w={90}
      />
    </Group>
  );
}

function numberOrEmpty(value: string | number): NumberField {
  return value === '' ? '' : Number(value);
}

export interface LifecycleRuleEditorProps {
  opened: boolean;
  /** The rule edited; null for a new rule. */
  rule: LifecycleRule | null;
  /** Ids of the other rules (unique ids). */
  otherIds: readonly string[];
  saving: boolean;
  /** What the server said about this rule on the last save. */
  serverProblems: readonly RuleProblem[];
  /** Another failure of the last save. */
  serverError?: string | undefined;
  readOnly?: boolean;
  onSave: (rule: LifecycleRule) => void;
  onClose: () => void;
}

/**
 * D-BKT-03 / TD-U-08: guided editor of one lifecycle rule (filter by
 * prefix, tags and size; expiration in days, at a date or of the expired
 * delete markers; noncurrent versions; incomplete uploads) with its JSON
 * and S3 XML views kept in sync with the form.
 */
export function LifecycleRuleEditor({
  opened,
  rule,
  otherIds,
  saving,
  serverProblems,
  serverError,
  readOnly = false,
  onSave,
  onClose,
}: LifecycleRuleEditorProps) {
  const { t } = useTranslation();
  const [form, setForm] = useState<LifecycleRuleForm>(emptyRuleForm);
  const [view, setView] = useState<RuleEditorView>('form');
  const [code, setCode] = useState('');
  const [codeError, setCodeError] = useState<string | null>(null);
  const [submitted, setSubmitted] = useState(false);

  useEffect(() => {
    if (!opened) return;
    setForm(rule === null ? emptyRuleForm() : ruleToForm(rule));
    setView('form');
    setCodeError(null);
    setSubmitted(false);
  }, [opened, rule]);

  const issues: RuleFormIssues = validateRuleForm(form, otherIds);
  const shown: RuleFormIssues = submitted ? issues : {};
  const issueText = (issue: RuleIssue | undefined) =>
    issue === undefined ? undefined : t(`lifecycle.issues.${issue}`);
  const patch = (next: Partial<LifecycleRuleForm>) => {
    setForm((current) => ({ ...current, ...next }));
  };

  const switchView = (next: RuleEditorView) => {
    if (next !== 'form') {
      const current = formToRule(form);
      setCode(next === 'json' ? ruleToJson(current) : ruleToXml(current));
      setCodeError(null);
    }
    setView(next);
  };
  const editCode = (text: string) => {
    setCode(text);
    const parsed = view === 'json' ? ruleFromJson(text) : ruleFromXml(text);
    if (parsed.ok) {
      setForm(ruleToForm(parsed.rule));
      setCodeError(null);
    } else {
      setCodeError(parsed.error);
    }
  };

  const submit = () => {
    setSubmitted(true);
    if (codeError !== null || !isValidRuleForm(issues)) return;
    onSave(formToRule(form));
  };

  const title =
    rule === null
      ? t('lifecycle.editor.titleNew')
      : t('lifecycle.editor.titleEdit', { id: rule.id ?? '' });

  return (
    <Modal opened={opened} onClose={onClose} title={title} size="lg" centered>
      <form
        noValidate
        onSubmit={(event) => {
          event.preventDefault();
          submit();
        }}
      >
        <Stack gap="md">
          <SegmentedControl
            aria-label={t('lifecycle.editor.view')}
            value={view}
            onChange={(next) => {
              switchView(next as RuleEditorView);
            }}
            data={[
              { value: 'form', label: t('lifecycle.editor.form') },
              { value: 'json', label: 'JSON' },
              { value: 'xml', label: 'XML' },
            ]}
          />
          {view === 'form' ? (
            <Stack gap="md">
              <Group align="flex-start" gap="md" wrap="wrap">
                <TextInput
                  label={t('lifecycle.editor.id')}
                  description={t('lifecycle.editor.idHint')}
                  value={form.id}
                  disabled={readOnly}
                  error={issueText(shown.id)}
                  onChange={(event) => {
                    patch({ id: event.currentTarget.value });
                  }}
                  style={{ flex: 1, minWidth: 200 }}
                />
                <Switch
                  mt={28}
                  label={t('lifecycle.editor.enabled')}
                  checked={form.enabled}
                  disabled={readOnly}
                  onChange={(event) => {
                    patch({ enabled: event.currentTarget.checked });
                  }}
                />
              </Group>

              <Fieldset legend={t('lifecycle.editor.filter')}>
                <Stack gap="sm">
                  <Text size="xs" c="dimmed">
                    {t('lifecycle.editor.filterHint')}
                  </Text>
                  <TextInput
                    label={t('lifecycle.editor.prefix')}
                    placeholder="logs/"
                    value={form.prefix}
                    disabled={readOnly}
                    error={issueText(shown.prefix)}
                    onChange={(event) => {
                      patch({ prefix: event.currentTarget.value });
                    }}
                  />
                  <Stack gap={4}>
                    <Text size="sm" fw={500}>
                      {t('lifecycle.editor.tags')}
                    </Text>
                    {form.tags.map((tag, i) => (
                      <Group key={i} gap="xs" wrap="nowrap" align="flex-start">
                        <TextInput
                          aria-label={t('lifecycle.editor.tagKey', { n: i + 1 })}
                          placeholder={t('lifecycle.editor.tagKeyPlaceholder')}
                          value={tag.key}
                          disabled={readOnly}
                          onChange={(event) => {
                            const key = event.currentTarget.value;
                            patch({
                              tags: form.tags.map((x, j) => (j === i ? { ...x, key } : x)),
                            });
                          }}
                          style={{ flex: 1 }}
                        />
                        <TextInput
                          aria-label={t('lifecycle.editor.tagValue', { n: i + 1 })}
                          placeholder={t('lifecycle.editor.tagValuePlaceholder')}
                          value={tag.value}
                          disabled={readOnly}
                          onChange={(event) => {
                            const value = event.currentTarget.value;
                            patch({
                              tags: form.tags.map((x, j) => (j === i ? { ...x, value } : x)),
                            });
                          }}
                          style={{ flex: 1 }}
                        />
                        <ActionIcon
                          variant="subtle"
                          color="red"
                          size="lg"
                          disabled={readOnly}
                          aria-label={t('lifecycle.editor.removeTag', { n: i + 1 })}
                          onClick={() => {
                            patch({ tags: form.tags.filter((_, j) => j !== i) });
                          }}
                        >
                          <IconTrash size={16} />
                        </ActionIcon>
                      </Group>
                    ))}
                    {shown.tags !== undefined ? (
                      <Text size="xs" c="red" role="alert">
                        {issueText(shown.tags)}
                      </Text>
                    ) : null}
                    <Group>
                      <Button
                        size="xs"
                        variant="subtle"
                        leftSection={<IconPlus size={14} />}
                        disabled={readOnly}
                        onClick={() => {
                          patch({ tags: [...form.tags, { key: '', value: '' }] });
                        }}
                      >
                        {t('lifecycle.editor.addTag')}
                      </Button>
                    </Group>
                  </Stack>
                  <Group grow align="flex-start" wrap="wrap">
                    <SizeField
                      label={t('lifecycle.editor.sizeGreater')}
                      value={form.sizeGreaterThan}
                      disabled={readOnly}
                      error={issueText(shown.sizeGreaterThan)}
                      onChange={(sizeGreaterThan) => {
                        patch({ sizeGreaterThan });
                      }}
                    />
                    <SizeField
                      label={t('lifecycle.editor.sizeLess')}
                      value={form.sizeLessThan}
                      disabled={readOnly}
                      error={issueText(shown.sizeLessThan)}
                      onChange={(sizeLessThan) => {
                        patch({ sizeLessThan });
                      }}
                    />
                  </Group>
                </Stack>
              </Fieldset>

              <Fieldset legend={t('lifecycle.editor.current')}>
                <Stack gap="sm">
                  <Select
                    label={t('lifecycle.editor.expiration')}
                    value={form.expiration}
                    allowDeselect={false}
                    disabled={readOnly}
                    data={(['none', 'days', 'date', 'deleteMarker'] as const).map((value) => ({
                      value,
                      label: t(`lifecycle.editor.expirationKinds.${value}`),
                    }))}
                    onChange={(next) => {
                      if (next !== null) patch({ expiration: next as ExpirationKind });
                    }}
                  />
                  {form.expiration === 'days' ? (
                    <NumberInput
                      label={t('lifecycle.editor.days')}
                      min={1}
                      clampBehavior="none"
                      allowDecimal={false}
                      allowNegative={false}
                      value={form.expirationDays}
                      disabled={readOnly}
                      error={issueText(shown.expirationDays)}
                      onChange={(next) => {
                        patch({ expirationDays: numberOrEmpty(next) });
                      }}
                    />
                  ) : null}
                  {form.expiration === 'date' ? (
                    <TextInput
                      type="date"
                      label={t('lifecycle.editor.date')}
                      description={t('lifecycle.editor.dateHint')}
                      value={form.expirationDate}
                      disabled={readOnly}
                      error={issueText(shown.expirationDate)}
                      onChange={(event) => {
                        patch({ expirationDate: event.currentTarget.value });
                      }}
                    />
                  ) : null}
                  {form.expiration === 'deleteMarker' ? (
                    <Text size="xs" c="dimmed">
                      {t('lifecycle.editor.deleteMarkerHint')}
                    </Text>
                  ) : null}
                </Stack>
              </Fieldset>

              <Fieldset legend={t('lifecycle.editor.noncurrentTitle')}>
                <Stack gap="sm">
                  <Switch
                    label={t('lifecycle.editor.noncurrent')}
                    checked={form.noncurrent}
                    disabled={readOnly}
                    onChange={(event) => {
                      patch({ noncurrent: event.currentTarget.checked });
                    }}
                  />
                  {form.noncurrent ? (
                    <Group grow align="flex-start" wrap="wrap">
                      <NumberInput
                        label={t('lifecycle.editor.noncurrentDays')}
                        min={1}
                        clampBehavior="none"
                        allowDecimal={false}
                        allowNegative={false}
                        value={form.noncurrentDays}
                        disabled={readOnly}
                        error={issueText(shown.noncurrentDays)}
                        onChange={(next) => {
                          patch({ noncurrentDays: numberOrEmpty(next) });
                        }}
                      />
                      <NumberInput
                        label={t('lifecycle.editor.newerVersions')}
                        description={t('lifecycle.editor.newerVersionsHint')}
                        min={1}
                        clampBehavior="none"
                        max={100}
                        allowDecimal={false}
                        allowNegative={false}
                        value={form.newerNoncurrentVersions}
                        disabled={readOnly}
                        error={issueText(shown.newerNoncurrentVersions)}
                        onChange={(next) => {
                          patch({ newerNoncurrentVersions: numberOrEmpty(next) });
                        }}
                      />
                    </Group>
                  ) : null}
                </Stack>
              </Fieldset>

              <Fieldset legend={t('lifecycle.editor.abortTitle')}>
                <Stack gap="sm">
                  <Switch
                    label={t('lifecycle.editor.abortUploads')}
                    checked={form.abortUploads}
                    disabled={readOnly}
                    onChange={(event) => {
                      patch({ abortUploads: event.currentTarget.checked });
                    }}
                  />
                  {form.abortUploads ? (
                    <NumberInput
                      label={t('lifecycle.editor.abortDays')}
                      min={1}
                      clampBehavior="none"
                      allowDecimal={false}
                      allowNegative={false}
                      value={form.abortDays}
                      disabled={readOnly}
                      error={issueText(shown.abortDays)}
                      onChange={(next) => {
                        patch({ abortDays: numberOrEmpty(next) });
                      }}
                    />
                  ) : null}
                </Stack>
              </Fieldset>
              {form.transitions !== undefined || form.noncurrentTransitions !== undefined ? (
                <Text size="xs" c="dimmed">
                  {t('lifecycle.editor.transitionsKept')}
                </Text>
              ) : null}
            </Stack>
          ) : (
            <CodeEditor
              label={
                view === 'json' ? t('lifecycle.editor.jsonLabel') : t('lifecycle.editor.xmlLabel')
              }
              description={t('lifecycle.editor.codeHint')}
              language={view}
              value={code}
              height={320}
              readOnly={readOnly}
              onChange={editCode}
              error={codeError ?? undefined}
            />
          )}

          {submitted && shown.rule !== undefined ? (
            <Alert color="red" variant="light" role="alert">
              {issueText(shown.rule)}
            </Alert>
          ) : null}
          {submitted && view !== 'form' && !isValidRuleForm(issues) && shown.rule === undefined ? (
            <Alert color="red" variant="light" role="alert">
              {t('lifecycle.editor.fixInForm')}
            </Alert>
          ) : null}
          {serverProblems.length > 0 || serverError !== undefined ? (
            <Alert color="red" variant="light" role="alert" title={t('lifecycle.serverRefused')}>
              <Stack gap={2}>
                {serverProblems.map((p) => (
                  <Text key={`${String(p.rule)}-${p.code}`} size="sm">
                    {p.message} ({p.code})
                  </Text>
                ))}
                {serverError !== undefined ? <Text size="sm">{serverError}</Text> : null}
              </Stack>
            </Alert>
          ) : null}

          <Group justify="flex-end" gap="xs">
            <Button variant="default" onClick={onClose}>
              {t('common.cancel')}
            </Button>
            {!readOnly ? (
              <Button type="submit" loading={saving}>
                {t('lifecycle.editor.save')}
              </Button>
            ) : null}
          </Group>
        </Stack>
      </form>
    </Modal>
  );
}
