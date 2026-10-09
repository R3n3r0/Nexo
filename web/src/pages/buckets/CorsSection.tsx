import {
  ActionIcon,
  Alert,
  Button,
  Checkbox,
  Group,
  NumberInput,
  Paper,
  Skeleton,
  Stack,
  Text,
  Textarea,
  TextInput,
  Title,
  Tooltip,
} from '@mantine/core';
import { notifications } from '@mantine/notifications';
import { IconPlus, IconTrash } from '@tabler/icons-react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';

import { StorageActions } from '@/auth/usePermissions';
import { PermissionGuard } from '@/components/iam/PermissionGuard';
import {
  corsFormToRule,
  corsLimits,
  corsMethods,
  corsRuleToForm,
  emptyCorsForm,
  isValidCorsRule,
  validateCorsRule,
  type CorsIssue,
  type CorsMethod,
  type CorsRuleForm,
} from '@/lib/cors';
import { problemMessage } from '@/lib/problem';
import { storageKeys } from '@/lib/storageKeys';
import { isApiError, useServices, type RuleProblem } from '@/services';

function ruleProblemsOf(error: unknown): readonly RuleProblem[] {
  return isApiError(error) ? error.ruleErrors : [];
}

/** The editor of one CORS rule. */
function CorsRuleCard({
  index,
  form,
  canEdit,
  submitted,
  problems,
  onChange,
  onRemove,
}: {
  index: number;
  form: CorsRuleForm;
  canEdit: boolean;
  submitted: boolean;
  problems: readonly RuleProblem[];
  onChange: (form: CorsRuleForm) => void;
  onRemove: () => void;
}) {
  const { t } = useTranslation();
  const issues = submitted ? validateCorsRule(corsFormToRule(form)) : {};
  const text = (issue: CorsIssue | undefined) =>
    issue === undefined ? undefined : t(`cors.issues.${issue}`);
  const n = index + 1;
  return (
    <Paper withBorder radius="sm" p="sm" component="fieldset" aria-label={t('cors.rule', { n })}>
      <Stack gap="xs">
        <Group justify="space-between" wrap="nowrap">
          <Text fw={500} size="sm">
            {t('cors.rule', { n })}
          </Text>
          {canEdit ? (
            <Tooltip label={t('cors.removeRule', { n })}>
              <ActionIcon
                variant="subtle"
                color="red"
                aria-label={t('cors.removeRule', { n })}
                onClick={onRemove}
              >
                <IconTrash size={16} />
              </ActionIcon>
            </Tooltip>
          ) : null}
        </Group>
        <Group grow align="flex-start" wrap="wrap">
          <Textarea
            label={t('cors.origins')}
            description={t('cors.originsHint')}
            autosize
            minRows={2}
            value={form.origins}
            disabled={!canEdit}
            error={text(issues.origins)}
            onChange={(event) => {
              onChange({ ...form, origins: event.currentTarget.value });
            }}
          />
          <Checkbox.Group
            label={t('cors.methods')}
            value={form.methods}
            error={text(issues.methods)}
            onChange={(values) => {
              onChange({ ...form, methods: values as CorsMethod[] });
            }}
          >
            <Group gap="sm" mt={6}>
              {corsMethods.map((m) => (
                <Checkbox key={m} value={m} label={m} disabled={!canEdit} />
              ))}
            </Group>
          </Checkbox.Group>
        </Group>
        <Group grow align="flex-start" wrap="wrap">
          <Textarea
            label={t('cors.allowedHeaders')}
            description={t('cors.headersHint')}
            autosize
            minRows={1}
            value={form.allowedHeaders}
            disabled={!canEdit}
            error={text(issues.allowedHeaders)}
            onChange={(event) => {
              onChange({ ...form, allowedHeaders: event.currentTarget.value });
            }}
          />
          <Textarea
            label={t('cors.exposeHeaders')}
            description={t('cors.headersHint')}
            autosize
            minRows={1}
            value={form.exposeHeaders}
            disabled={!canEdit}
            onChange={(event) => {
              onChange({ ...form, exposeHeaders: event.currentTarget.value });
            }}
          />
        </Group>
        <Group grow align="flex-start" wrap="wrap">
          <NumberInput
            label={t('cors.maxAge')}
            min={0}
            allowDecimal={false}
            allowNegative={false}
            value={form.maxAgeSeconds}
            disabled={!canEdit}
            error={text(issues.maxAgeSeconds)}
            onChange={(next) => {
              onChange({ ...form, maxAgeSeconds: next === '' ? '' : Number(next) });
            }}
          />
          <TextInput
            label={t('cors.id')}
            description={t('cors.idHint')}
            value={form.id}
            disabled={!canEdit}
            error={text(issues.id)}
            onChange={(event) => {
              onChange({ ...form, id: event.currentTarget.value });
            }}
          />
        </Group>
        {problems.map((p) => (
          <Text key={`${p.code}-${p.message}`} size="sm" c="red" role="alert">
            {p.message} ({p.code})
          </Text>
        ))}
      </Stack>
    </Paper>
  );
}

/** D-BKT-02: the CORS rules of a bucket, evaluated in order. */
export function CorsSection({ bucket, canEdit }: { bucket: string; canEdit: boolean }) {
  const { t } = useTranslation();
  const { buckets } = useServices();
  const queryClient = useQueryClient();
  const key = storageKeys.bucketSetting(bucket, 'cors');
  const query = useQuery({ queryKey: key, queryFn: () => buckets.getCors(bucket) });
  const [forms, setForms] = useState<CorsRuleForm[]>([]);
  const [submitted, setSubmitted] = useState(false);
  useEffect(() => {
    if (query.data !== undefined) {
      setForms(query.data.rules.map(corsRuleToForm));
      setSubmitted(false);
    }
  }, [query.data]);

  const save = useMutation({
    mutationFn: (next: CorsRuleForm[]) =>
      buckets.setCors(bucket, { rules: next.map(corsFormToRule) }),
    onSuccess: (stored) => {
      queryClient.setQueryData(key, stored);
      notifications.show({ color: 'green', message: t('cors.saved') });
    },
  });
  const problems = ruleProblemsOf(save.error);
  const valid = forms.every((f) => isValidCorsRule(corsFormToRule(f)));
  const dirty =
    query.data !== undefined &&
    JSON.stringify(forms.map(corsFormToRule)) !== JSON.stringify(query.data.rules);

  return (
    <Paper withBorder radius="md" p="md" component="section" aria-label={t('cors.title')}>
      <Stack gap="sm">
        <Stack gap={2}>
          <Title order={4}>{t('cors.title')}</Title>
          <Text size="sm" c="dimmed">
            {t('cors.description')}
          </Text>
        </Stack>
        {query.isPending ? (
          <Skeleton height={60} />
        ) : query.isError ? (
          <Text size="sm" c="dimmed">
            {isApiError(query.error) && query.error.isForbidden
              ? t('cors.unavailable')
              : problemMessage(t, query.error)}
          </Text>
        ) : (
          <form
            noValidate
            onSubmit={(event) => {
              event.preventDefault();
              setSubmitted(true);
              if (valid) save.mutate(forms);
            }}
          >
            <Stack gap="sm">
              {forms.length === 0 ? (
                <Text size="sm" c="dimmed">
                  {t('cors.empty')}
                </Text>
              ) : null}
              {forms.map((form, i) => (
                <CorsRuleCard
                  key={i}
                  index={i}
                  form={form}
                  canEdit={canEdit}
                  submitted={submitted}
                  problems={problems.filter((p) => p.rule === i + 1)}
                  onChange={(next) => {
                    setForms((current) => current.map((f, j) => (j === i ? next : f)));
                  }}
                  onRemove={() => {
                    setForms((current) => current.filter((_, j) => j !== i));
                  }}
                />
              ))}
              {save.isError && problems.every((p) => p.rule === 0) ? (
                <Alert color="red" variant="light" role="alert">
                  {problems[0]?.message ?? problemMessage(t, save.error)}
                </Alert>
              ) : null}
              <Group justify="space-between" wrap="wrap">
                {canEdit ? (
                  <Button
                    variant="subtle"
                    leftSection={<IconPlus size={14} />}
                    disabled={forms.length >= corsLimits.rules}
                    onClick={() => {
                      setForms((current) => [...current, emptyCorsForm()]);
                    }}
                  >
                    {t('cors.addRule')}
                  </Button>
                ) : (
                  <span />
                )}
                <PermissionGuard allowed={canEdit} action={StorageActions.putCors}>
                  {(disabled) => (
                    <Button type="submit" disabled={disabled || !dirty} loading={save.isPending}>
                      {t('cors.save')}
                    </Button>
                  )}
                </PermissionGuard>
              </Group>
            </Stack>
          </form>
        )}
      </Stack>
    </Paper>
  );
}
