import { Badge, Group, Stack, TagsInput, Text } from '@mantine/core';
import { useQuery } from '@tanstack/react-query';
import { useEffect } from 'react';
import { useTranslation } from 'react-i18next';

import { distributionKeys } from '@/lib/accessQueries';
import {
  aliasIssue,
  aliasState,
  isWildcardAlias,
  normalizeAlias,
  type AliasState,
} from '@/lib/distributions';
import { useServices } from '@/services';

export interface AliasesFieldProps {
  value: string[];
  onChange: (aliases: string[]) => void;
  /** The distribution being edited: its own aliases are not conflicts. */
  distribution?: string | undefined;
  disabled?: boolean;
  error?: string | undefined;
  /** Called when an alias is taken (another distribution, the S3/console domain), or no longer. */
  onConflict?: (conflict: boolean) => void;
}

const stateColor: Record<AliasState, string> = {
  invalid: 'red',
  available: 'teal',
  mine: 'blue',
  taken: 'red',
  main: 'red',
  checking: 'gray',
};

/**
 * D-DST-02: the domains of a distribution (exact or *.wildcard), validated
 * like the server and checked against the routing table: an alias served
 * by another distribution, or the S3/console domain, is reported before
 * the save (which answers 409 alias_conflict).
 */
export function AliasesField({
  value,
  onChange,
  distribution,
  disabled = false,
  error,
  onConflict,
}: AliasesFieldProps) {
  const { t } = useTranslation();
  const { distributions } = useServices();
  const aliases = value.map(normalizeAlias);
  // One query for every alias (the routing table of the answering node).
  const valid = aliases.filter((a) => aliasIssue(a) === undefined);
  const routes = useQuery({
    queryKey: distributionKeys.resolve(valid.join(',')),
    queryFn: async () =>
      Promise.all(
        valid.map((alias) =>
          distributions.resolve(isWildcardAlias(alias) ? `check${alias.slice(1)}` : alias),
        ),
      ),
    enabled: valid.length > 0,
    staleTime: 5_000,
  });
  const routeOf = (alias: string) => {
    const i = valid.indexOf(alias);
    return i < 0 || routes.data === undefined || routes.data.length !== valid.length
      ? undefined
      : routes.data[i];
  };
  const states = aliases.map((alias) => aliasState(alias, routeOf(alias), distribution));
  const conflict = states.some((s) => s === 'taken' || s === 'main');
  useEffect(() => {
    onConflict?.(conflict);
  }, [conflict, onConflict]);
  return (
    <Stack gap={6}>
      <TagsInput
        label={t('distributions.fields.aliases')}
        description={t('distributions.fields.aliasesHint')}
        placeholder="www.example.com"
        value={value}
        disabled={disabled}
        acceptValueOnBlur
        splitChars={[',', ' ']}
        error={error}
        onChange={(next) => {
          onChange(
            next.map(normalizeAlias).filter((a, i, all) => a !== '' && all.indexOf(a) === i),
          );
        }}
      />
      {aliases.length > 0 ? (
        <Stack gap={2} role="list" aria-label={t('distributions.aliasStates')}>
          {aliases.map((alias, i) => {
            const state = states[i] ?? 'checking';
            const route = routeOf(alias);
            return (
              <Group key={alias} gap="xs" role="listitem">
                <Badge size="sm" color={stateColor[state]} variant="light">
                  {t(`distributions.aliasState.${state}`)}
                </Badge>
                <Text size="sm" ff="monospace">
                  {alias}
                </Text>
                {state === 'taken' ? (
                  <Text size="xs" c="red">
                    {t('distributions.aliasTaken', { id: route?.distribution ?? '' })}
                  </Text>
                ) : state === 'invalid' ? (
                  <Text size="xs" c="red">
                    {t(`distributions.aliasIssue.${aliasIssue(alias) ?? 'invalid'}`)}
                  </Text>
                ) : null}
              </Group>
            );
          })}
        </Stack>
      ) : null}
    </Stack>
  );
}
