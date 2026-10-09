import { Alert, Badge, Button, Checkbox, Group, Paper, Stack, Text, Title } from '@mantine/core';
import { IconAlertTriangle, IconWorld } from '@tabler/icons-react';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';

import { StorageActions } from '@/auth/usePermissions';
import { ChoiceField } from '@/components/common/ChoiceField';
import { PermissionGuard } from '@/components/iam/PermissionGuard';
import type { AccessPresetInput, AccessRulesPublicStatus, KeyPattern } from '@/services';

import { patternIssue } from '@/lib/keyPattern';

import { PatternField } from './PatternField';

const presets = [
  'private',
  'public-read',
  'public-read-list',
  'public-prefix',
  'public-write',
] as const;
type SettablePreset = (typeof presets)[number];

export interface PresetPanelProps {
  status: AccessRulesPublicStatus | undefined;
  /** Block public access in effect on the bucket. */
  blocked: boolean;
  canEdit: boolean;
  applying: boolean;
  onApply: (input: AccessPresetInput) => void;
}

/**
 * D-ACC-03: the public presets of a bucket (private, public-read,
 * public-read-list, public-prefix with a pattern, public-write with an
 * explicit confirmation) with clear warnings and the current public state.
 * A preset replaces only the rules it generated (ids preset-*).
 */
export function PresetPanel({ status, blocked, canEdit, applying, onApply }: PresetPanelProps) {
  const { t } = useTranslation();
  const [preset, setPreset] = useState<SettablePreset>('public-read');
  const [match, setMatch] = useState<KeyPattern | undefined>({
    type: 'prefix',
    pattern: 'public/',
  });
  const [confirmed, setConfirmed] = useState(false);
  const needsMatch = preset === 'public-prefix';
  const matchInvalid =
    needsMatch &&
    (match === undefined || match.pattern === '' || patternIssue(match) !== undefined);
  const disabled = !canEdit || matchInvalid || (preset === 'public-write' && !confirmed);

  return (
    <Paper withBorder radius="md" p="md" component="section" aria-label={t('presets.title')}>
      <Stack gap="sm">
        <Group justify="space-between" wrap="wrap" gap="xs">
          <Title order={4}>{t('presets.title')}</Title>
          {status !== undefined ? (
            <Group gap={4}>
              <Badge
                leftSection={<IconWorld size={12} />}
                color={status.public ? 'orange' : 'gray'}
                variant="light"
              >
                {status.public ? t('presets.statePublic') : t('presets.statePrivate')}
              </Badge>
              <Badge variant="outline" color="gray">
                {t(`presets.names.${status.preset}`)}
              </Badge>
              {status.blocked && (status.rules?.length ?? 0) > 0 ? (
                <Badge color="grape" variant="light">
                  {t('presets.blocked')}
                </Badge>
              ) : null}
            </Group>
          ) : null}
        </Group>
        <Text size="sm" c="dimmed">
          {t('presets.description')}
        </Text>
        <ChoiceField
          label={t('presets.choose')}
          value={preset}
          orientation="vertical"
          disabled={!canEdit}
          choices={presets.map((p) => ({
            value: p,
            label: t(`presets.names.${p}`),
            description: t(`presets.help.${p}`),
          }))}
          onChange={(value) => {
            setPreset(value);
            setConfirmed(false);
          }}
        />
        {needsMatch ? (
          <PatternField
            label={t('presets.pattern')}
            value={match}
            disabled={!canEdit}
            placeholders={{ prefix: 'public/', glob: 'pub/**', regex: '^pub/' }}
            onChange={setMatch}
          />
        ) : null}
        {preset !== 'private' ? (
          <Alert
            color={preset === 'public-write' ? 'red' : 'orange'}
            variant="light"
            icon={<IconAlertTriangle size={18} />}
          >
            {t(`presets.warnings.${preset}`)}
          </Alert>
        ) : null}
        {preset === 'public-write' ? (
          <Checkbox
            label={t('presets.confirmWrite')}
            checked={confirmed}
            disabled={!canEdit}
            onChange={(event) => {
              setConfirmed(event.currentTarget.checked);
            }}
          />
        ) : null}
        {blocked && preset !== 'private' ? (
          <Alert color="grape" variant="light">
            {t('presets.blockedWarning')}
          </Alert>
        ) : null}
        <Group>
          <PermissionGuard allowed={canEdit} action={StorageActions.manageAccessRules}>
            {(guarded) => (
              <Button
                color={preset === 'public-write' ? 'red' : undefined}
                disabled={guarded || disabled}
                loading={applying}
                onClick={() => {
                  onApply({
                    preset,
                    ...(needsMatch && match !== undefined ? { match } : {}),
                    ...(preset === 'public-write' ? { confirm: confirmed } : {}),
                  });
                }}
              >
                {t('presets.apply', { preset: t(`presets.names.${preset}`) })}
              </Button>
            )}
          </PermissionGuard>
        </Group>
      </Stack>
    </Paper>
  );
}
