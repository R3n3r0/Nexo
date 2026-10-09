import {
  Alert,
  Badge,
  Button,
  Code,
  CopyButton,
  Group,
  Stack,
  Table,
  Text,
  TextInput,
  Tooltip,
  ActionIcon,
} from '@mantine/core';
import { IconCheck, IconCopy, IconRoute } from '@tabler/icons-react';
import { useMutation } from '@tanstack/react-query';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';

import { dnsRecords, isWildcardAlias } from '@/lib/distributions';
import { problemMessage } from '@/lib/problem';
import { useServices, type Route } from '@/services';

export interface DnsInstructionsProps {
  aliases: readonly string[];
  /** The distribution the aliases must route to, once created. */
  distribution?: string;
}

/** The host the console is served from: the default target of the records. */
function consoleHost(): string {
  return typeof window !== 'undefined' ? window.location.hostname : '';
}

/** A probe host of a wildcard alias (*.preview.example.com → check.preview.example.com). */
function probeHost(alias: string): string {
  return isWildcardAlias(alias) ? `check${alias.slice(1)}` : alias;
}

/**
 * D-DST-02: the DNS records pointing the aliases to the cluster (or to the
 * proxy in front of it): CNAME for a host name, A/AAAA for the apex of a
 * zone or an address target, and the verification that every alias is
 * routed to the distribution by the routing table of the cluster
 * (GET /routing/resolve).
 */
export function DnsInstructions({ aliases, distribution }: DnsInstructionsProps) {
  const { t } = useTranslation();
  const { distributions } = useServices();
  const [target, setTarget] = useState(consoleHost);
  const verify = useMutation({
    mutationFn: async () => {
      const out: { alias: string; route: Route }[] = [];
      for (const alias of aliases) {
        out.push({ alias, route: await distributions.resolve(probeHost(alias)) });
      }
      return out;
    },
  });
  const records = aliases.flatMap((a) => dnsRecords(a, target));
  return (
    <Stack gap="sm">
      <TextInput
        label={t('dns.target')}
        description={t('dns.targetHint')}
        value={target}
        onChange={(event) => {
          setTarget(event.currentTarget.value);
        }}
        maw={420}
      />
      {records.length === 0 ? (
        <Text size="sm" c="dimmed">
          {t('dns.none')}
        </Text>
      ) : (
        <Table.ScrollContainer minWidth={520}>
          <Table aria-label={t('dns.records')} verticalSpacing={4}>
            <Table.Thead>
              <Table.Tr>
                <Table.Th>{t('dns.name')}</Table.Th>
                <Table.Th>{t('dns.type')}</Table.Th>
                <Table.Th>{t('dns.value')}</Table.Th>
                <Table.Th w={48} />
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {records.map((r) => (
                <Table.Tr key={`${r.name}-${r.type}`}>
                  <Table.Td>
                    <Code>{r.name}</Code>
                  </Table.Td>
                  <Table.Td>
                    <Badge variant="outline" color="gray">
                      {r.type}
                    </Badge>
                  </Table.Td>
                  <Table.Td>
                    <Code>{r.value}</Code>
                  </Table.Td>
                  <Table.Td>
                    <CopyButton value={`${r.name} ${r.type} ${r.value}`}>
                      {({ copied, copy }) => (
                        <Tooltip label={copied ? t('common.copied') : t('common.copy')}>
                          <ActionIcon
                            variant="subtle"
                            aria-label={t('dns.copy', { name: r.name, type: r.type })}
                            onClick={copy}
                          >
                            {copied ? <IconCheck size={14} /> : <IconCopy size={14} />}
                          </ActionIcon>
                        </Tooltip>
                      )}
                    </CopyButton>
                  </Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        </Table.ScrollContainer>
      )}
      <Text size="xs" c="dimmed">
        {t('dns.apexHint')}
      </Text>
      {distribution !== undefined ? (
        <Stack gap="xs">
          <Group>
            <Button
              variant="default"
              leftSection={<IconRoute size={16} />}
              loading={verify.isPending}
              disabled={aliases.length === 0}
              onClick={() => {
                verify.mutate();
              }}
            >
              {t('dns.verify')}
            </Button>
          </Group>
          {verify.isError ? (
            <Alert color="red" variant="light" role="alert">
              {problemMessage(t, verify.error)}
            </Alert>
          ) : null}
          {verify.data !== undefined ? (
            <Stack gap={4} aria-live="polite" aria-label={t('dns.verification')} role="list">
              {verify.data.map(({ alias, route }) => {
                const ok = route.kind === 'distribution' && route.distribution === distribution;
                return (
                  <Group key={alias} gap="xs" role="listitem">
                    <Badge color={ok ? 'teal' : 'red'} variant="light">
                      {ok ? t('dns.routed') : t('dns.notRouted')}
                    </Badge>
                    <Text size="sm" ff="monospace">
                      {alias}
                    </Text>
                    <Text size="xs" c="dimmed">
                      {route.kind === 'distribution'
                        ? t('dns.servedBy', {
                            id: route.distribution ?? '',
                            alias: route.alias ?? '',
                          })
                        : t(`dns.kinds.${route.kind}`)}
                    </Text>
                  </Group>
                );
              })}
              <Text size="xs" c="dimmed">
                {t('dns.verifyHint')}
              </Text>
            </Stack>
          ) : null}
        </Stack>
      ) : null}
    </Stack>
  );
}
