import {
  ActionIcon,
  Button,
  Group,
  NumberInput,
  Paper,
  Select,
  Stack,
  Switch,
  TagsInput,
  Text,
  TextInput,
  Title,
} from '@mantine/core';
import { IconPlus, IconShieldCheck, IconTrash } from '@tabler/icons-react';
import { useTranslation } from 'react-i18next';

import { KeyValueEditor } from '@/components/common/KeyValueEditor';
import { securityHeadersPreset } from '@/lib/distributions';
import type {
  Distribution,
  DistributionCors,
  DistributionErrorPage,
  DistributionRedirect,
} from '@/services';

export interface ResponsesPanelProps {
  draft: Distribution;
  onChange: (next: Distribution) => void;
  readOnly: boolean;
}

type RedirectKind = 'path' | 'host' | 'https';

function kindOf(r: DistributionRedirect): RedirectKind {
  if (r.httpsOnly === true) return 'https';
  if ((r.hostFrom ?? '') !== '') return 'host';
  return 'path';
}

const statuses = ['301', '302', '307', '308'];

/**
 * D-DST-04: how a website distribution answers beyond the objects: error
 * pages per status, redirects (path regex with groups, host, HTTP to
 * HTTPS), SPA fallback, CORS and the response headers, with a recommended
 * set of security headers.
 */
export function ResponsesPanel({ draft, onChange, readOnly }: ResponsesPanelProps) {
  const { t } = useTranslation();
  const errorPages = draft.errorPages ?? [];
  const redirects = draft.redirects ?? [];
  const response = draft.headers?.response ?? {};
  const cors = draft.headers?.cors;
  const spa = (draft.defaultBehavior?.spaFallback ?? '') !== '';
  const website = (draft.mode ?? 'website') === 'website';

  const setPages = (next: DistributionErrorPage[]) => {
    onChange({ ...draft, errorPages: next });
  };
  const setRedirects = (next: DistributionRedirect[]) => {
    onChange({ ...draft, redirects: next });
  };
  const setHeaders = (patch: {
    response?: Record<string, string>;
    cors?: DistributionCors | undefined;
  }) => {
    const headers = { ...draft.headers, ...patch };
    if (patch.cors === undefined && 'cors' in patch) delete headers.cors;
    onChange({ ...draft, headers });
  };

  return (
    <Stack gap="md">
      {!website ? (
        <Text size="sm" c="dimmed">
          {t('distributions.responses.s3Mode')}
        </Text>
      ) : null}
      <Paper
        withBorder
        radius="md"
        p="md"
        component="section"
        aria-label={t('distributions.responses.errorPages')}
      >
        <Stack gap="sm">
          <Title order={4}>{t('distributions.responses.errorPages')}</Title>
          <Text size="sm" c="dimmed">
            {t('distributions.responses.errorPagesHint')}
          </Text>
          {errorPages.map((p, i) => (
            <Group key={i} gap="xs" align="flex-end" wrap="wrap">
              <NumberInput
                label={t('distributions.responses.status')}
                min={400}
                max={599}
                allowDecimal={false}
                value={p.status}
                disabled={readOnly}
                w={110}
                onChange={(value) => {
                  setPages(
                    errorPages.map((x, j) => (j === i ? { ...x, status: Number(value) } : x)),
                  );
                }}
              />
              <TextInput
                label={t('distributions.responses.path')}
                placeholder="/404.html"
                value={p.path}
                disabled={readOnly}
                style={{ flex: '1 1 200px' }}
                onChange={(event) => {
                  const path = event.currentTarget.value;
                  setPages(errorPages.map((x, j) => (j === i ? { ...x, path } : x)));
                }}
              />
              <NumberInput
                label={t('distributions.responses.responseStatus')}
                min={100}
                max={599}
                allowDecimal={false}
                value={p.responseStatus ?? ''}
                disabled={readOnly}
                w={150}
                onChange={(value) => {
                  setPages(
                    errorPages.map((x, j) => {
                      if (j !== i) return x;
                      const { responseStatus: _r, ...rest } = x;
                      return value === '' ? rest : { ...rest, responseStatus: Number(value) };
                    }),
                  );
                }}
              />
              <ActionIcon
                variant="subtle"
                color="red"
                size="lg"
                disabled={readOnly}
                aria-label={t('distributions.responses.removeErrorPage', { status: p.status })}
                onClick={() => {
                  setPages(errorPages.filter((_, j) => j !== i));
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
              disabled={readOnly}
              onClick={() => {
                setPages([...errorPages, { status: 404, path: '/404.html', responseStatus: 404 }]);
              }}
            >
              {t('distributions.responses.addErrorPage')}
            </Button>
          </Group>
          <Switch
            label={t('distributions.responses.spa')}
            description={t('distributions.responses.spaHint')}
            checked={spa}
            disabled={readOnly || !website}
            onChange={(event) => {
              const on = event.currentTarget.checked;
              const { spaFallback: _s, ...rest } = draft.defaultBehavior ?? {};
              onChange({
                ...draft,
                defaultBehavior: on ? { ...rest, spaFallback: '/index.html' } : rest,
              });
            }}
          />
        </Stack>
      </Paper>

      <Paper
        withBorder
        radius="md"
        p="md"
        component="section"
        aria-label={t('distributions.responses.redirects')}
      >
        <Stack gap="sm">
          <Title order={4}>{t('distributions.responses.redirects')}</Title>
          <Text size="sm" c="dimmed">
            {t('distributions.responses.redirectsHint')}
          </Text>
          {redirects.map((r, i) => {
            const kind = kindOf(r);
            const update = (next: DistributionRedirect) => {
              setRedirects(redirects.map((x, j) => (j === i ? next : x)));
            };
            return (
              <Group key={i} gap="xs" align="flex-end" wrap="wrap">
                <Select
                  label={t('distributions.responses.redirectKind')}
                  data={(['path', 'host', 'https'] as const).map((k) => ({
                    value: k,
                    label: t(`distributions.responses.kinds.${k}`),
                  }))}
                  value={kind}
                  allowDeselect={false}
                  disabled={readOnly}
                  w={170}
                  onChange={(value) => {
                    const status = r.status ?? 301;
                    if (value === 'https') update({ httpsOnly: true, status });
                    else if (value === 'host') update({ hostFrom: '', toHost: '', status });
                    else update({ from: '', to: '', status });
                  }}
                />
                {kind === 'path' ? (
                  <>
                    <TextInput
                      label={t('distributions.responses.from')}
                      placeholder="^/old/(.*)$"
                      value={r.from ?? ''}
                      disabled={readOnly}
                      style={{ flex: '1 1 180px' }}
                      styles={{ input: { fontFamily: 'var(--mantine-font-family-monospace)' } }}
                      onChange={(event) => {
                        update({ ...r, from: event.currentTarget.value });
                      }}
                    />
                    <TextInput
                      label={t('distributions.responses.to')}
                      placeholder="/new/$1"
                      value={r.to ?? ''}
                      disabled={readOnly}
                      style={{ flex: '1 1 160px' }}
                      onChange={(event) => {
                        update({ ...r, to: event.currentTarget.value });
                      }}
                    />
                  </>
                ) : kind === 'host' ? (
                  <>
                    <TextInput
                      label={t('distributions.responses.hostFrom')}
                      placeholder="example.com"
                      value={r.hostFrom ?? ''}
                      disabled={readOnly}
                      style={{ flex: '1 1 160px' }}
                      onChange={(event) => {
                        update({ ...r, hostFrom: event.currentTarget.value });
                      }}
                    />
                    <TextInput
                      label={t('distributions.responses.toHost')}
                      placeholder="www.example.com"
                      value={r.toHost ?? ''}
                      disabled={readOnly}
                      style={{ flex: '1 1 160px' }}
                      onChange={(event) => {
                        update({ ...r, toHost: event.currentTarget.value });
                      }}
                    />
                  </>
                ) : (
                  <Text size="sm" style={{ flex: '1 1 300px' }} pb={8}>
                    {t('distributions.responses.httpsHint')}
                  </Text>
                )}
                <Select
                  label={t('distributions.responses.status')}
                  data={statuses}
                  value={String(r.status ?? 301)}
                  allowDeselect={false}
                  disabled={readOnly}
                  w={100}
                  onChange={(value) => {
                    update({ ...r, status: Number(value ?? 301) });
                  }}
                />
                <ActionIcon
                  variant="subtle"
                  color="red"
                  size="lg"
                  disabled={readOnly}
                  aria-label={t('distributions.responses.removeRedirect', { n: i + 1 })}
                  onClick={() => {
                    setRedirects(redirects.filter((_, j) => j !== i));
                  }}
                >
                  <IconTrash size={16} />
                </ActionIcon>
              </Group>
            );
          })}
          <Group>
            <Button
              size="xs"
              variant="default"
              leftSection={<IconPlus size={14} />}
              disabled={readOnly || !website}
              onClick={() => {
                setRedirects([...redirects, { from: '', to: '', status: 301 }]);
              }}
            >
              {t('distributions.responses.addRedirect')}
            </Button>
          </Group>
        </Stack>
      </Paper>

      <Paper
        withBorder
        radius="md"
        p="md"
        component="section"
        aria-label={t('distributions.responses.headers')}
      >
        <Stack gap="sm">
          <Group justify="space-between" wrap="wrap">
            <Title order={4}>{t('distributions.responses.headers')}</Title>
            <Button
              size="xs"
              variant="light"
              leftSection={<IconShieldCheck size={14} />}
              disabled={readOnly}
              onClick={() => {
                setHeaders({ response: { ...response, ...securityHeadersPreset } });
              }}
            >
              {t('distributions.responses.securityPreset')}
            </Button>
          </Group>
          <Text size="sm" c="dimmed">
            {t('distributions.responses.headersHint')}
          </Text>
          <KeyValueEditor
            label={t('distributions.fields.responseHeaders')}
            keyLabel={t('distributions.fields.headerName')}
            valueLabel={t('distributions.fields.headerValue')}
            keyPlaceholder="Strict-Transport-Security"
            valuePlaceholder="max-age=63072000"
            value={response}
            disabled={readOnly}
            onChange={(next) => {
              setHeaders({ response: next });
            }}
          />
          <TagsInput
            label={t('distributions.fields.corsOrigins')}
            description={t('distributions.responses.corsHint')}
            placeholder="https://app.example.com"
            value={cors?.allowedOrigins ?? []}
            disabled={readOnly}
            acceptValueOnBlur
            onChange={(allowedOrigins) => {
              setHeaders({
                cors: allowedOrigins.length === 0 ? undefined : { ...(cors ?? {}), allowedOrigins },
              });
            }}
          />
          {cors !== undefined ? (
            <Group gap="sm" align="flex-end" wrap="wrap">
              <TagsInput
                label={t('distributions.fields.corsMethods')}
                data={['GET', 'HEAD', 'OPTIONS', 'POST', 'PUT']}
                value={cors.allowedMethods ?? []}
                disabled={readOnly}
                style={{ flex: '1 1 260px' }}
                onChange={(allowedMethods) => {
                  setHeaders({ cors: { ...cors, allowedMethods } });
                }}
              />
              <NumberInput
                label={t('distributions.fields.corsMaxAge')}
                min={0}
                allowDecimal={false}
                value={cors.maxAge ?? ''}
                disabled={readOnly}
                w={180}
                onChange={(value) => {
                  const { maxAge: _m, ...rest } = cors;
                  setHeaders({ cors: value === '' ? rest : { ...rest, maxAge: Number(value) } });
                }}
              />
            </Group>
          ) : null}
        </Stack>
      </Paper>
    </Stack>
  );
}
