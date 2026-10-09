import {
  Accordion,
  Alert,
  Button,
  Checkbox,
  Group,
  Modal,
  MultiSelect,
  NumberInput,
  SegmentedControl,
  Select,
  SimpleGrid,
  Stack,
  TagsInput,
  Text,
  Textarea,
  TextInput,
} from '@mantine/core';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';

import { KeyValueEditor } from '@/components/common/KeyValueEditor';
import {
  actionClasses,
  cleanRule,
  conditionMethods,
  hasIssues,
  validateRule,
} from '@/lib/accessRules';
import type { AccessRuleConditions, AccessRuleProblem, BucketAccessRule } from '@/services';

import { PatternField } from './PatternField';
import { PatternPreview } from './PatternPreview';

export interface RuleEditorProps {
  /** The rule edited (a new rule has an id no other rule has). */
  rule: BucketAccessRule;
  /** True when the rule is not stored yet. */
  isNew: boolean;
  /** The other rules of the bucket (unique ids). */
  others: readonly BucketAccessRule[];
  /** Keys of the bucket offered to the preview. */
  sampleKeys: readonly string[];
  saving: boolean;
  /** What the server located in this rule on the last save. */
  serverProblems: readonly AccessRuleProblem[];
  /** Another failure of the last save. */
  serverError?: string | undefined;
  readOnly?: boolean;
  onSave: (rule: BucketAccessRule) => void;
  onClose: () => void;
}

type Secure = 'any' | 'required' | 'forbidden';

const principalHints = ['*', 'user:', 'group:', 'accesskey:', 'distribution:'];

/**
 * D-ACC-01: editor of one access rule (priority, effect, principals,
 * actions, prefix/glob/regex pattern and conditions) with a live preview
 * of the pattern on keys of the bucket (TD-A-01). The pattern is checked
 * as it is typed; the server validates the whole rule on save and its
 * problems are shown on the fields, the position in the pattern included.
 */
export function RuleEditor({
  rule,
  isNew,
  others,
  sampleKeys,
  saving,
  serverProblems,
  serverError,
  readOnly = false,
  onSave,
  onClose,
}: RuleEditorProps) {
  const { t } = useTranslation();
  const [draft, setDraft] = useState<BucketAccessRule>(rule);
  const [submitted, setSubmitted] = useState(false);
  // The sample keys follow the keys of the bucket until edited.
  const [typedKeys, setKeysText] = useState<string | null>(null);
  const keysText = typedKeys ?? sampleKeys.join('\n');
  const issues = validateRule(draft, others);
  const conditions: AccessRuleConditions = draft.conditions ?? {};
  const classes = draft.actions.filter((a) => (actionClasses as readonly string[]).includes(a));
  const explicit = draft.actions.filter((a) => !(actionClasses as readonly string[]).includes(a));
  const secure: Secure =
    conditions.secureTransport === undefined
      ? 'any'
      : conditions.secureTransport
        ? 'required'
        : 'forbidden';
  const keys = keysText
    .split('\n')
    .map((k) => k.trim())
    .filter((k) => k !== '');

  const set = (patch: Partial<BucketAccessRule>) => {
    setDraft((d) => ({ ...d, ...patch }));
  };
  const setConditions = (patch: Partial<AccessRuleConditions>) => {
    setDraft((d) => ({ ...d, conditions: { ...(d.conditions ?? {}), ...patch } }));
  };

  const patternProblem = serverProblems.find((p) => p.field === 'match.pattern');
  const otherProblems = serverProblems.filter((p) => p !== patternProblem);
  const show = submitted || !isNew;
  const fieldError = (key: 'id' | 'priority' | 'principals' | 'actions'): string | undefined => {
    if (!show) return undefined;
    switch (key) {
      case 'id':
        return issues.id === undefined ? undefined : t(`accessRules.editor.issues.id.${issues.id}`);
      case 'priority':
        return issues.priority === undefined ? undefined : t('accessRules.editor.issues.priority');
      case 'principals':
        return issues.principals === undefined
          ? undefined
          : t(`accessRules.editor.issues.principals.${issues.principals}`);
      case 'actions':
        return issues.actions === undefined ? undefined : t('accessRules.editor.issues.actions');
    }
  };

  return (
    <Modal
      opened
      onClose={onClose}
      size="xl"
      title={
        readOnly
          ? t('accessRules.editor.viewTitle', { id: rule.id })
          : isNew
            ? t('accessRules.editor.newTitle')
            : t('accessRules.editor.editTitle', { id: rule.id })
      }
    >
      <form
        onSubmit={(event) => {
          event.preventDefault();
          setSubmitted(true);
          if (!readOnly && !hasIssues(issues)) onSave(cleanRule(draft));
        }}
        noValidate
      >
        <Stack gap="md">
          <SimpleGrid cols={{ base: 1, sm: 3 }} spacing="sm">
            <TextInput
              label={t('accessRules.fields.id')}
              required
              value={draft.id}
              disabled={readOnly}
              error={fieldError('id')}
              onChange={(event) => {
                set({ id: event.currentTarget.value });
              }}
            />
            <NumberInput
              label={t('accessRules.fields.priority')}
              description={t('accessRules.editor.priorityHint')}
              min={0}
              max={1_000_000}
              allowDecimal={false}
              allowNegative={false}
              value={draft.priority}
              disabled={readOnly}
              error={fieldError('priority')}
              onChange={(next) => {
                set({ priority: next === '' ? -1 : Number(next) });
              }}
            />
            <Stack gap={4}>
              <Text size="sm" fw={500} id="rule-effect-label">
                {t('accessRules.fields.effect')}
              </Text>
              <SegmentedControl
                aria-labelledby="rule-effect-label"
                value={draft.effect}
                disabled={readOnly}
                data={[
                  { value: 'allow', label: t('accessRules.effects.allow') },
                  { value: 'deny', label: t('accessRules.effects.deny') },
                ]}
                color={draft.effect === 'deny' ? 'red' : 'teal'}
                onChange={(value) => {
                  set({ effect: value === 'deny' ? 'deny' : 'allow' });
                }}
              />
            </Stack>
          </SimpleGrid>

          <TagsInput
            label={t('accessRules.fields.principals')}
            description={t('accessRules.editor.principalsHint')}
            data={principalHints}
            value={draft.principals}
            disabled={readOnly}
            acceptValueOnBlur
            clearable
            error={fieldError('principals')}
            onChange={(principals) => {
              set({ principals });
            }}
          />
          {draft.effect === 'allow' && draft.principals.includes('*') ? (
            <Alert color="orange" variant="light">
              {t('accessRules.editor.publicWarning')}
            </Alert>
          ) : null}

          <Checkbox.Group
            label={t('accessRules.fields.actions')}
            description={t('accessRules.editor.actionsHint')}
            value={classes}
            error={fieldError('actions')}
            onChange={(next) => {
              set({ actions: [...next, ...explicit] });
            }}
          >
            <Group gap="md" mt={6}>
              {actionClasses.map((a) => (
                <Checkbox
                  key={a}
                  value={a}
                  label={t(`accessRules.actions.${a}`)}
                  disabled={readOnly}
                />
              ))}
            </Group>
          </Checkbox.Group>
          <TagsInput
            label={t('accessRules.editor.s3Actions')}
            description={t('accessRules.editor.s3ActionsHint')}
            placeholder="s3:GetObject"
            value={explicit}
            disabled={readOnly}
            acceptValueOnBlur
            onChange={(next) => {
              set({ actions: [...classes, ...next] });
            }}
          />

          <PatternField
            label={t('accessRules.fields.pattern')}
            value={draft.match}
            allowNone
            disabled={readOnly}
            serverIssue={
              patternProblem !== undefined
                ? { position: patternProblem.position, message: patternProblem.message }
                : undefined
            }
            placeholders={{
              prefix: 'public/',
              glob: 'docs/**/*.pdf',
              regex: '^img/[0-9]{4}/.*\\.png$',
            }}
            onChange={(match) => {
              setDraft((d) => {
                const { match: _old, ...rest } = d;
                return match === undefined ? rest : { ...rest, match };
              });
            }}
          />

          <SimpleGrid cols={{ base: 1, md: 2 }} spacing="md">
            <Textarea
              label={t('accessRules.editor.sampleKeys')}
              description={t('accessRules.editor.sampleKeysHint')}
              autosize
              minRows={4}
              maxRows={10}
              value={keysText}
              spellCheck={false}
              styles={{ input: { fontFamily: 'var(--mantine-font-family-monospace)' } }}
              onChange={(event) => {
                setKeysText(event.currentTarget.value);
              }}
            />
            <Stack gap={4}>
              <Text size="sm" fw={500}>
                {t('accessRules.editor.preview')}
              </Text>
              <PatternPreview
                pattern={draft.match}
                keys={keys}
                label={t('accessRules.editor.previewList')}
              />
            </Stack>
          </SimpleGrid>

          <Accordion
            variant="contained"
            defaultValue={draft.conditions !== undefined ? 'conditions' : null}
          >
            <Accordion.Item value="conditions">
              <Accordion.Control>{t('accessRules.editor.conditions')}</Accordion.Control>
              <Accordion.Panel>
                <Stack gap="sm">
                  <SimpleGrid cols={{ base: 1, sm: 2 }} spacing="sm">
                    <TagsInput
                      label={t('accessRules.conditions.sourceIp')}
                      placeholder="10.0.0.0/8"
                      value={conditions.sourceIp ?? []}
                      disabled={readOnly}
                      acceptValueOnBlur
                      error={
                        show && issues.sourceIp !== undefined
                          ? t('accessRules.editor.issues.ip', { value: issues.sourceIp })
                          : undefined
                      }
                      onChange={(sourceIp) => {
                        setConditions({ sourceIp });
                      }}
                    />
                    <TagsInput
                      label={t('accessRules.conditions.notSourceIp')}
                      placeholder="203.0.113.0/24"
                      value={conditions.notSourceIp ?? []}
                      disabled={readOnly}
                      acceptValueOnBlur
                      error={
                        show && issues.notSourceIp !== undefined
                          ? t('accessRules.editor.issues.ip', { value: issues.notSourceIp })
                          : undefined
                      }
                      onChange={(notSourceIp) => {
                        setConditions({ notSourceIp });
                      }}
                    />
                    <TagsInput
                      label={t('accessRules.conditions.referer')}
                      placeholder="https://*.example.com/*"
                      value={conditions.referer ?? []}
                      disabled={readOnly}
                      acceptValueOnBlur
                      onChange={(referer) => {
                        setConditions({ referer });
                      }}
                    />
                    <TagsInput
                      label={t('accessRules.conditions.host')}
                      placeholder="cdn.example.com"
                      value={conditions.host ?? []}
                      disabled={readOnly}
                      acceptValueOnBlur
                      onChange={(host) => {
                        setConditions({ host });
                      }}
                    />
                    <MultiSelect
                      label={t('accessRules.conditions.methods')}
                      data={[...conditionMethods]}
                      value={conditions.methods ?? []}
                      disabled={readOnly}
                      onChange={(methods) => {
                        setConditions({ methods });
                      }}
                    />
                    <Select
                      label={t('accessRules.conditions.secureTransport')}
                      data={(['any', 'required', 'forbidden'] as const).map((v) => ({
                        value: v,
                        label: t(`accessRules.secure.${v}`),
                      }))}
                      value={secure}
                      allowDeselect={false}
                      disabled={readOnly}
                      onChange={(value) => {
                        setDraft((d) => {
                          const { secureTransport: _s, ...rest } = d.conditions ?? {};
                          return {
                            ...d,
                            conditions:
                              value === 'required' || value === 'forbidden'
                                ? { ...rest, secureTransport: value === 'required' }
                                : rest,
                          };
                        });
                      }}
                    />
                    <TextInput
                      label={t('accessRules.conditions.from')}
                      placeholder="2026-01-01T00:00:00Z"
                      value={conditions.timeWindow?.from ?? ''}
                      disabled={readOnly}
                      error={
                        show && issues.timeWindow !== undefined
                          ? t(`accessRules.editor.issues.timeWindow.${issues.timeWindow}`)
                          : undefined
                      }
                      onChange={(event) => {
                        setConditions({
                          timeWindow: { ...conditions.timeWindow, from: event.currentTarget.value },
                        });
                      }}
                    />
                    <TextInput
                      label={t('accessRules.conditions.to')}
                      placeholder="2026-12-31T23:59:59Z"
                      value={conditions.timeWindow?.to ?? ''}
                      disabled={readOnly}
                      onChange={(event) => {
                        setConditions({
                          timeWindow: { ...conditions.timeWindow, to: event.currentTarget.value },
                        });
                      }}
                    />
                  </SimpleGrid>
                  <KeyValueEditor
                    label={t('accessRules.conditions.headers')}
                    description={t('accessRules.editor.headersHint')}
                    keyLabel={t('accessRules.editor.headerName')}
                    valueLabel={t('accessRules.editor.headerValue')}
                    keyPlaceholder="X-Origin-Secret"
                    valuePlaceholder="${secret:proxy-token}"
                    value={conditions.headers ?? {}}
                    disabled={readOnly}
                    onChange={(headers) => {
                      setConditions({ headers });
                    }}
                  />
                  <KeyValueEditor
                    label={t('accessRules.conditions.captureEquals')}
                    description={t('accessRules.editor.captureHint')}
                    keyLabel={t('accessRules.editor.groupName')}
                    valueLabel={t('accessRules.editor.groupValue')}
                    keyPlaceholder="user"
                    valuePlaceholder="${aws:username}"
                    value={conditions.captureEquals ?? {}}
                    disabled={readOnly}
                    onChange={(captureEquals) => {
                      setConditions({ captureEquals });
                    }}
                  />
                </Stack>
              </Accordion.Panel>
            </Accordion.Item>
          </Accordion>

          <Textarea
            label={t('accessRules.fields.description')}
            autosize
            minRows={1}
            maxRows={4}
            value={draft.description ?? ''}
            disabled={readOnly}
            onChange={(event) => {
              set({ description: event.currentTarget.value });
            }}
          />

          {otherProblems.length > 0 ? (
            <Alert color="red" variant="light" role="alert" title={t('accessRules.serverRefused')}>
              <Stack gap={2}>
                {otherProblems.map((p) => (
                  <Text key={`${p.field ?? ''}-${p.message}`} size="sm">
                    {p.field !== undefined ? `${p.field}: ` : ''}
                    {p.message}
                  </Text>
                ))}
              </Stack>
            </Alert>
          ) : null}
          {serverError !== undefined ? (
            <Alert color="red" variant="light" role="alert">
              {serverError}
            </Alert>
          ) : null}
          {submitted && hasIssues(issues) ? (
            <Text size="sm" c="red" role="alert">
              {t('accessRules.editor.fixIssues')}
            </Text>
          ) : null}

          <Group justify="flex-end" gap="xs">
            <Button variant="default" onClick={onClose}>
              {readOnly ? t('common.close') : t('common.cancel')}
            </Button>
            {!readOnly ? (
              <Button type="submit" loading={saving}>
                {t('accessRules.editor.save')}
              </Button>
            ) : null}
          </Group>
        </Stack>
      </form>
    </Modal>
  );
}
