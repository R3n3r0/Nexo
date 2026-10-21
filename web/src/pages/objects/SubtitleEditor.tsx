import {
  ActionIcon,
  Alert,
  Button,
  Group,
  Modal,
  ScrollArea,
  Skeleton,
  Stack,
  Table,
  Text,
  Textarea,
  TextInput,
  Tooltip,
} from '@mantine/core';
import { IconPlus, IconTrash } from '@tabler/icons-react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';

import { ErrorAlert } from '@/components/iam/common';
import { cueProblem, formatCueTime, parseCueTime } from '@/lib/subtitles';
import { storageKeys } from '@/lib/storageKeys';
import { useServices, type SubtitleCue, type SubtitleCues } from '@/services';

/** A cue being edited: its times as typed. */
interface Row {
  id: number;
  start: string;
  end: string;
  text: string;
  settings?: string | undefined;
}

let nextRow = 0;

function toRow(c: SubtitleCue): Row {
  nextRow += 1;
  return {
    id: nextRow,
    start: formatCueTime(c.start),
    end: formatCueTime(c.end),
    text: c.text,
    settings: c.settings,
  };
}

/** Why a row is invalid. */
type RowProblem = 'time' | 'order' | 'text' | 'long';

/** The cue of a row, or the reason it is invalid. */
function rowCue(row: Row): { cue?: SubtitleCue; problem?: RowProblem } {
  const start = parseCueTime(row.start);
  const end = parseCueTime(row.end);
  if (start === undefined || end === undefined) return { problem: 'time' };
  const cue: SubtitleCue = { start, end, text: row.text.trim() };
  if (row.settings !== undefined && row.settings !== '') cue.settings = row.settings;
  const problem = cueProblem(cue);
  return problem === undefined ? { cue } : { problem };
}

function CueTable({
  initial,
  saving,
  error,
  automatic,
  onSave,
  onCancel,
}: {
  initial: SubtitleCues;
  saving: boolean;
  error: unknown;
  automatic: boolean;
  onSave: (cues: SubtitleCue[]) => void;
  onCancel: () => void;
}) {
  const { t } = useTranslation();
  const [rows, setRows] = useState<Row[]>(() => initial.cues.map(toRow));
  const checked = rows.map(rowCue);
  const invalid = checked.some((c) => c.problem !== undefined) || rows.length === 0;
  const update = (id: number, patch: Partial<Row>) => {
    setRows((rs) => rs.map((r) => (r.id === id ? { ...r, ...patch } : r)));
  };
  const add = () => {
    setRows((rs) => {
      const last = rs[rs.length - 1];
      const start = last !== undefined ? (parseCueTime(last.end) ?? 0) : 0;
      return [...rs, toRow({ start, end: start + 2, text: '' })];
    });
  };

  return (
    <Stack gap="sm">
      {automatic ? (
        <Alert color="blue" variant="light">
          {t('subtitles.editAutomatic')}
        </Alert>
      ) : null}
      <ScrollArea.Autosize mah="60vh">
        <Table aria-label={t('subtitles.cues')} verticalSpacing={4} withTableBorder>
          <Table.Thead>
            <Table.Tr>
              <Table.Th w={130}>{t('subtitles.start')}</Table.Th>
              <Table.Th w={130}>{t('subtitles.end')}</Table.Th>
              <Table.Th>{t('subtitles.text')}</Table.Th>
              <Table.Th w={40}>
                <Text size="sm" fw={700} style={{ visibility: 'hidden' }}>
                  {t('common.actions')}
                </Text>
              </Table.Th>
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {rows.map((row, i) => {
              const problem = checked[i]?.problem;
              const n = i + 1;
              return (
                <Table.Tr key={row.id}>
                  <Table.Td>
                    <TextInput
                      size="xs"
                      value={row.start}
                      aria-label={t('subtitles.startOf', { n })}
                      error={problem === 'time' && parseCueTime(row.start) === undefined}
                      onChange={(e) => {
                        update(row.id, { start: e.currentTarget.value });
                      }}
                    />
                  </Table.Td>
                  <Table.Td>
                    <TextInput
                      size="xs"
                      value={row.end}
                      aria-label={t('subtitles.endOf', { n })}
                      error={
                        problem === 'order' ||
                        (problem === 'time' && parseCueTime(row.end) === undefined)
                      }
                      onChange={(e) => {
                        update(row.id, { end: e.currentTarget.value });
                      }}
                    />
                  </Table.Td>
                  <Table.Td>
                    <Textarea
                      size="xs"
                      autosize
                      minRows={1}
                      maxRows={4}
                      value={row.text}
                      aria-label={t('subtitles.textOf', { n })}
                      error={problem !== undefined ? t(`subtitles.problem.${problem}`) : undefined}
                      onChange={(e) => {
                        update(row.id, { text: e.currentTarget.value });
                      }}
                    />
                  </Table.Td>
                  <Table.Td>
                    <Tooltip label={t('subtitles.removeCue', { n })}>
                      <ActionIcon
                        variant="subtle"
                        color="red"
                        aria-label={t('subtitles.removeCue', { n })}
                        onClick={() => {
                          setRows((rs) => rs.filter((r) => r.id !== row.id));
                        }}
                      >
                        <IconTrash size={16} />
                      </ActionIcon>
                    </Tooltip>
                  </Table.Td>
                </Table.Tr>
              );
            })}
          </Table.Tbody>
        </Table>
      </ScrollArea.Autosize>
      <Group justify="space-between">
        <Button size="xs" variant="default" leftSection={<IconPlus size={14} />} onClick={add}>
          {t('subtitles.addCue')}
        </Button>
        <Text size="xs" c="dimmed">
          {t('subtitles.timeHint')}
        </Text>
      </Group>
      {error !== undefined && error !== null ? <ErrorAlert error={error} /> : null}
      <Group justify="flex-end" gap="xs">
        <Button variant="default" onClick={onCancel} disabled={saving}>
          {t('common.cancel')}
        </Button>
        <Button
          disabled={invalid}
          loading={saving}
          onClick={() => {
            const cues = checked.flatMap((c) => (c.cue === undefined ? [] : [c.cue]));
            onSave(cues.sort((a, b) => a.start - b.start));
          }}
        >
          {t('common.save')}
        </Button>
      </Group>
    </Stack>
  );
}

export interface SubtitleEditorProps {
  bucket: string;
  objectKey: string;
  versionId?: string | undefined;
  /** The language of the track edited; undefined closes the editor. */
  lang: string | undefined;
  onClose: () => void;
}

/**
 * Edits the cues of a subtitle track: the times and the text of each cue,
 * cues added or removed. Saving stores a manual track (an automatic one
 * becomes manual: the media worker then leaves it alone).
 */
export function SubtitleEditor({
  bucket,
  objectKey,
  versionId,
  lang,
  onClose,
}: SubtitleEditorProps) {
  const { t } = useTranslation();
  const { media } = useServices();
  const queryClient = useQueryClient();
  const query = useQuery({
    queryKey: storageKeys.subtitleCues(bucket, objectKey, lang ?? '', versionId),
    queryFn: () => media.subtitleCues(bucket, objectKey, lang ?? '', versionId),
    enabled: lang !== undefined,
    gcTime: 0,
  });
  const save = useMutation({
    mutationFn: (cues: SubtitleCue[]) =>
      media.putSubtitleCues(bucket, objectKey, lang ?? '', cues, versionId),
    onSuccess: async () => {
      await queryClient.invalidateQueries({
        queryKey: storageKeys.subtitles(bucket, objectKey, versionId),
      });
      onClose();
    },
  });

  return (
    <Modal
      opened={lang !== undefined}
      onClose={onClose}
      size="xl"
      title={
        query.data !== undefined
          ? t('subtitles.editTitle', { name: query.data.track.name, lang: query.data.track.lang })
          : t('subtitles.edit')
      }
    >
      {query.isPending ? <Skeleton height={160} /> : null}
      {query.isError ? <ErrorAlert error={query.error} /> : null}
      {query.data !== undefined ? (
        <CueTable
          key={`${query.data.track.lang}-${String(query.dataUpdatedAt)}`}
          initial={query.data}
          automatic={query.data.track.auto}
          saving={save.isPending}
          error={save.error}
          onSave={(cues) => {
            save.mutate(cues);
          }}
          onCancel={onClose}
        />
      ) : null}
    </Modal>
  );
}
