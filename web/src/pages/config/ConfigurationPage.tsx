import {
  Accordion,
  Alert,
  Badge,
  Button,
  FileButton,
  Group,
  Loader,
  Stack,
  Text,
} from '@mantine/core';
import { notifications } from '@mantine/notifications';
import { IconDownload, IconUpload } from '@tabler/icons-react';
import { useMutation, useQuery } from '@tanstack/react-query';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';

import { StorageActions, usePermissions } from '@/auth/usePermissions';
import { BlockPublicControl } from '@/components/accessRules/BlockPublicControl';
import { ConfirmDialog, ErrorAlert, PageHeader } from '@/components/iam/common';
import { checkConfigValue } from '@/lib/config';
import { iamKeys, useInvalidatingMutation } from '@/lib/iamQueries';
import { parseJson } from '@/lib/json';
import { readTextFile, saveJsonFile } from '@/lib/saveFile';
import {
  useServices,
  type ConfigDocument,
  type ConfigSettings,
  type ConfigSubsystemSchema,
} from '@/services';

import { ConfigField } from './ConfigField';
import { KmsSection } from './KmsSection';
import { NotifyTargetsSection } from './NotifyTargetsSection';
import { ObservabilitySection } from './ObservabilitySection';

/** D-ACC-04: the block public access of the cluster, with its mode. */
function ClusterBlockPublic() {
  const perms = usePermissions();
  return <BlockPublicControl canEdit={perms.can(StorageActions.manageAccessRules)} />;
}

/** Validates an imported document: subsystems of string settings. */
function asConfigDocument(value: unknown): ConfigDocument | undefined {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return undefined;
  const out: ConfigDocument = {};
  for (const [name, settings] of Object.entries(value)) {
    if (typeof settings !== 'object' || settings === null || Array.isArray(settings))
      return undefined;
    const kv: ConfigSettings = {};
    for (const [key, v] of Object.entries(settings as Record<string, unknown>)) {
      if (typeof v !== 'string') return undefined;
      kv[key] = v;
    }
    out[name] = kv;
  }
  return out;
}

/**
 * Configuration page (D-CFG-01): the notification targets (notify_*) with
 * a form per type and their state, the KMS, the observability (logger,
 * audit targets, tracing and TLS state), one generic key/value form per
 * subsystem from GET /config/schema, export and import of the whole
 * configuration.
 */
export function ConfigurationPage() {
  const { t } = useTranslation();
  const { config } = useServices();
  const [importing, setImporting] = useState<{ name: string; document: ConfigDocument } | null>(
    null,
  );
  const [importError, setImportError] = useState<string | null>(null);

  const schema = useQuery({ queryKey: iamKeys.configSchema, queryFn: () => config.schema() });
  const values = useQuery({ queryKey: iamKeys.config, queryFn: () => config.list() });

  const exportConfig = useMutation({
    mutationFn: () => config.exportAll(),
    onSuccess: (document) => {
      saveJsonFile('nexo-config.json', document);
    },
  });
  const importConfig = useInvalidatingMutation({
    mutationFn: (document: ConfigDocument) => config.importAll(document),
    invalidate: [iamKeys.config],
    onSuccess: () => {
      setImporting(null);
      notifications.show({ color: 'teal', message: t('configuration.imported') });
    },
  });

  const readImport = async (file: File | null) => {
    setImportError(null);
    if (file === null) return;
    const parsed = parseJson(await readTextFile(file));
    const document = parsed.ok ? asConfigDocument(parsed.value) : undefined;
    if (document === undefined) {
      setImportError(t('configuration.importInvalid', { file: file.name }));
      return;
    }
    importConfig.reset();
    setImporting({ name: file.name, document });
  };

  const error = schema.error ?? values.error;

  return (
    <Stack gap="lg">
      <PageHeader
        title={t('configuration.title')}
        description={t('configuration.subtitle')}
        actions={
          <>
            <Button
              variant="default"
              leftSection={<IconDownload size={16} />}
              loading={exportConfig.isPending}
              onClick={() => {
                exportConfig.mutate();
              }}
            >
              {t('configuration.export')}
            </Button>
            <FileButton onChange={(file) => void readImport(file)} accept="application/json,.json">
              {(props) => (
                <Button variant="default" leftSection={<IconUpload size={16} />} {...props}>
                  {t('configuration.import')}
                </Button>
              )}
            </FileButton>
          </>
        }
      />
      <Alert variant="light" color="yellow">
        {t('configuration.exportWarning')}
      </Alert>
      <Text size="sm" c="dimmed">
        {t('configuration.appliedNote')}
      </Text>
      {exportConfig.isError ? <ErrorAlert error={exportConfig.error} /> : null}
      {importError !== null ? (
        <Alert color="red" variant="light" role="alert">
          {importError}
        </Alert>
      ) : null}
      {error !== null ? (
        <ErrorAlert
          error={error}
          title={t('configuration.loadError')}
          onRetry={() => {
            void schema.refetch();
            void values.refetch();
          }}
        />
      ) : null}
      <NotifyTargetsSection schema={schema.data} />
      <KmsSection />
      <ObservabilitySection schema={schema.data} values={values.data} />
      <ClusterBlockPublic />
      {schema.isPending || values.isPending ? <Loader aria-label={t('common.loading')} /> : null}
      {schema.data && values.data ? (
        <Accordion multiple variant="separated" defaultValue={['region']}>
          {schema.data.map((subsystem) => {
            const current = values.data.find((v) => v.name === subsystem.name)?.settings ?? {};
            return (
              <Accordion.Item key={subsystem.name} value={subsystem.name}>
                <Accordion.Control>
                  <Stack gap={0}>
                    <Group gap="xs">
                      <Text fw={500} ff="monospace">
                        {subsystem.name}
                      </Text>
                      {subsystem.keys.every((key) => !key.applied) ? (
                        <Badge size="xs" variant="light" color="gray">
                          {t('configuration.notApplied')}
                        </Badge>
                      ) : null}
                    </Group>
                    <Text size="sm" c="dimmed">
                      {subsystem.description}
                    </Text>
                  </Stack>
                </Accordion.Control>
                <Accordion.Panel>
                  <SubsystemForm
                    key={JSON.stringify(current)}
                    subsystem={subsystem}
                    settings={current}
                  />
                </Accordion.Panel>
              </Accordion.Item>
            );
          })}
        </Accordion>
      ) : null}
      <ConfirmDialog
        opened={importing !== null}
        title={t('configuration.importTitle')}
        message={t('configuration.importConfirm', { file: importing?.name ?? '' })}
        confirmLabel={t('configuration.import')}
        loading={importConfig.isPending}
        error={importConfig.isError ? importConfig.error : undefined}
        onCancel={() => {
          setImporting(null);
        }}
        onConfirm={() => {
          if (importing) importConfig.mutate(importing.document);
        }}
      />
    </Stack>
  );
}

function SubsystemForm({
  subsystem,
  settings,
}: {
  subsystem: ConfigSubsystemSchema;
  settings: ConfigSettings;
}) {
  const { t } = useTranslation();
  const { config } = useServices();
  const [draft, setDraft] = useState<ConfigSettings>(settings);
  const [resetting, setResetting] = useState(false);

  const changed = Object.fromEntries(
    Object.entries(draft).filter(([key, value]) => (settings[key] ?? '') !== value),
  );
  const problems = Object.fromEntries(
    subsystem.keys.flatMap((key) => {
      const problem = checkConfigValue(key, draft[key.name] ?? '');
      return problem !== undefined ? [[key.name, problem] as const] : [];
    }),
  );
  const valid = Object.keys(problems).length === 0;

  const save = useInvalidatingMutation({
    mutationFn: () => config.set(subsystem.name, changed),
    invalidate: [iamKeys.config],
    onSuccess: () => {
      notifications.show({
        color: 'teal',
        message: t('configuration.saved', { subsystem: subsystem.name }),
      });
    },
  });
  const reset = useInvalidatingMutation({
    mutationFn: () => config.reset(subsystem.name),
    invalidate: [iamKeys.config],
    onSuccess: () => {
      setResetting(false);
    },
  });

  const setValue = (name: string, value: string) => {
    setDraft((current) => ({ ...current, [name]: value }));
  };

  return (
    <Stack gap="sm">
      {save.isError ? <ErrorAlert error={save.error} /> : null}
      {subsystem.keys.map((key) => (
        <ConfigField
          key={key.name}
          configKey={key}
          value={draft[key.name] ?? ''}
          error={
            problems[key.name] !== undefined
              ? t(`configuration.problems.${problems[key.name] ?? 'list'}`, {
                  values: (key.values ?? []).join(', '),
                })
              : undefined
          }
          onChange={(value) => {
            setValue(key.name, value);
          }}
        />
      ))}
      <Group justify="space-between">
        <Button
          variant="subtle"
          color="red"
          onClick={() => {
            reset.reset();
            setResetting(true);
          }}
        >
          {t('configuration.reset')}
        </Button>
        <Button
          onClick={() => {
            save.mutate();
          }}
          disabled={!valid || Object.keys(changed).length === 0}
          loading={save.isPending}
        >
          {t('common.save')}
        </Button>
      </Group>
      <ConfirmDialog
        opened={resetting}
        title={t('configuration.resetTitle')}
        message={t('configuration.resetConfirm', { subsystem: subsystem.name })}
        confirmLabel={t('configuration.reset')}
        loading={reset.isPending}
        error={reset.isError ? reset.error : undefined}
        onCancel={() => {
          setResetting(false);
        }}
        onConfirm={() => {
          reset.mutate();
        }}
      />
    </Stack>
  );
}
