import {
  Alert,
  Badge,
  Button,
  Group,
  NumberInput,
  Paper,
  Select,
  SimpleGrid,
  Stack,
  Switch,
  TagsInput,
  Text,
  TextInput,
  Title,
} from '@mantine/core';
import { IconKey, IconRefresh } from '@tabler/icons-react';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';

import { isIpOrCidr } from '@/lib/accessRules';
import { defaultOriginSecretHeader } from '@/lib/distributions';
import type { Distribution, DistributionRestrictions, RotateOriginSecretInput } from '@/services';

export interface SecurityPanelProps {
  draft: Distribution;
  onChange: (next: Distribution) => void;
  readOnly: boolean;
  /** The draft has unsaved changes: the rotation is refused until saved. */
  dirty: boolean;
  rotating: boolean;
  onRotate: (input: RotateOriginSecretInput) => void;
}

/**
 * D-DST-06: the restrictions of a distribution: client addresses allowed
 * or denied (behind the trusted proxies), the origin secret every proxy
 * must send (generated and rotated by the server, several active during a
 * rotation) and the rate limit per client.
 */
export function SecurityPanel({
  draft,
  onChange,
  readOnly,
  dirty,
  rotating,
  onRotate,
}: SecurityPanelProps) {
  const { t } = useTranslation();
  const restrictions: DistributionRestrictions = draft.restrictions ?? {};
  const secret = restrictions.originSecretHeader;
  const [header, setHeader] = useState(secret?.name ?? defaultOriginSecretHeader);
  const [keep, setKeep] = useState<string>('1');
  const allow = restrictions.sourceIp?.allow ?? [];
  const deny = restrictions.sourceIp?.deny ?? [];
  const badAllow = allow.find((v) => !isIpOrCidr(v));
  const badDeny = deny.find((v) => !isIpOrCidr(v));
  const rate = restrictions.rateLimit;

  const setRestrictions = (next: DistributionRestrictions) => {
    onChange({ ...draft, restrictions: next });
  };

  return (
    <Stack gap="md">
      <Paper
        withBorder
        radius="md"
        p="md"
        component="section"
        aria-label={t('distributions.security.ipTitle')}
      >
        <Stack gap="sm">
          <Title order={4}>{t('distributions.security.ipTitle')}</Title>
          <Text size="sm" c="dimmed">
            {t('distributions.security.ipHint')}
          </Text>
          <SimpleGrid cols={{ base: 1, sm: 2 }} spacing="sm">
            <TagsInput
              label={t('distributions.security.allow')}
              placeholder="10.0.0.0/8"
              value={allow}
              disabled={readOnly}
              acceptValueOnBlur
              error={
                badAllow !== undefined
                  ? t('accessRules.editor.issues.ip', { value: badAllow })
                  : undefined
              }
              onChange={(next) => {
                setRestrictions({ ...restrictions, sourceIp: { allow: next, deny } });
              }}
            />
            <TagsInput
              label={t('distributions.security.deny')}
              placeholder="203.0.113.0/24"
              value={deny}
              disabled={readOnly}
              acceptValueOnBlur
              error={
                badDeny !== undefined
                  ? t('accessRules.editor.issues.ip', { value: badDeny })
                  : undefined
              }
              onChange={(next) => {
                setRestrictions({ ...restrictions, sourceIp: { allow, deny: next } });
              }}
            />
          </SimpleGrid>
        </Stack>
      </Paper>

      <Paper
        withBorder
        radius="md"
        p="md"
        component="section"
        aria-label={t('distributions.security.secretTitle')}
      >
        <Stack gap="sm">
          <Group justify="space-between" wrap="wrap">
            <Group gap="xs">
              <IconKey size={18} aria-hidden="true" />
              <Title order={4}>{t('distributions.security.secretTitle')}</Title>
            </Group>
            <Badge color={secret !== undefined ? 'teal' : 'gray'} variant="light">
              {secret !== undefined
                ? t('distributions.security.secretActive', {
                    count: (secret.values ?? []).length,
                    header: secret.name,
                  })
                : t('distributions.security.secretNone')}
            </Badge>
          </Group>
          <Text size="sm" c="dimmed">
            {t('distributions.security.secretHint')}
          </Text>
          <SimpleGrid cols={{ base: 1, sm: 3 }} spacing="sm">
            <TextInput
              label={t('distributions.security.header')}
              value={header}
              disabled={readOnly}
              onChange={(event) => {
                setHeader(event.currentTarget.value);
              }}
            />
            <Select
              label={t('distributions.security.keep')}
              description={t('distributions.security.keepHint')}
              data={['0', '1', '2', '3'].map((v) => ({
                value: v,
                label: t('distributions.security.keepN', { count: Number(v) }),
              }))}
              value={keep}
              allowDeselect={false}
              disabled={readOnly}
              onChange={(value) => {
                if (value !== null) setKeep(value);
              }}
            />
          </SimpleGrid>
          {dirty ? (
            <Alert color="yellow" variant="light">
              {t('distributions.security.saveFirst')}
            </Alert>
          ) : null}
          <Group gap="xs">
            <Button
              leftSection={<IconRefresh size={16} />}
              loading={rotating}
              disabled={readOnly || dirty || header.trim() === ''}
              onClick={() => {
                onRotate({
                  header: header.trim(),
                  keep: Number(keep),
                  ...(keep === '0' ? { dropPrevious: true } : {}),
                });
              }}
            >
              {secret !== undefined
                ? t('distributions.security.rotate')
                : t('distributions.security.generate')}
            </Button>
            {secret !== undefined && !readOnly ? (
              <Button
                variant="subtle"
                color="red"
                disabled={dirty}
                onClick={() => {
                  const { originSecretHeader: _o, ...rest } = restrictions;
                  setRestrictions(rest);
                }}
              >
                {t('distributions.security.removeSecret')}
              </Button>
            ) : null}
          </Group>
        </Stack>
      </Paper>

      <Paper
        withBorder
        radius="md"
        p="md"
        component="section"
        aria-label={t('distributions.security.rateTitle')}
      >
        <Stack gap="sm">
          <Group justify="space-between">
            <Title order={4}>{t('distributions.security.rateTitle')}</Title>
            <Switch
              aria-label={t('distributions.security.rateEnable')}
              label={t('distributions.security.rateEnable')}
              checked={rate !== undefined}
              disabled={readOnly}
              onChange={(event) => {
                const { rateLimit: _r, ...rest } = restrictions;
                setRestrictions(
                  event.currentTarget.checked
                    ? { ...rest, rateLimit: { requestsPerSecond: 200, burst: 400, per: 'ip' } }
                    : rest,
                );
              }}
            />
          </Group>
          <Text size="sm" c="dimmed">
            {t('distributions.security.rateHint')}
          </Text>
          {rate !== undefined ? (
            <SimpleGrid cols={{ base: 1, sm: 3 }} spacing="sm">
              <NumberInput
                label={t('distributions.security.rps')}
                min={1}
                value={rate.requestsPerSecond ?? ''}
                disabled={readOnly}
                onChange={(value) => {
                  setRestrictions({
                    ...restrictions,
                    rateLimit: { ...rate, requestsPerSecond: Number(value) },
                  });
                }}
              />
              <NumberInput
                label={t('distributions.security.burst')}
                min={1}
                allowDecimal={false}
                value={rate.burst ?? ''}
                disabled={readOnly}
                onChange={(value) => {
                  setRestrictions({
                    ...restrictions,
                    rateLimit: { ...rate, burst: Number(value) },
                  });
                }}
              />
              <Select
                label={t('distributions.security.per')}
                data={[
                  { value: 'ip', label: t('distributions.security.perIp') },
                  { value: 'distribution', label: t('distributions.security.perDistribution') },
                ]}
                value={rate.per ?? 'ip'}
                allowDeselect={false}
                disabled={readOnly}
                onChange={(value) => {
                  setRestrictions({
                    ...restrictions,
                    rateLimit: { ...rate, per: value === 'distribution' ? 'distribution' : 'ip' },
                  });
                }}
              />
            </SimpleGrid>
          ) : null}
        </Stack>
      </Paper>
    </Stack>
  );
}
