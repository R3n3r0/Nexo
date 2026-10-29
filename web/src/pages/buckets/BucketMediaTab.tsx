import {
  Accordion,
  ActionIcon,
  Alert,
  Badge,
  Button,
  Checkbox,
  Group,
  Modal,
  MultiSelect,
  NumberInput,
  Paper,
  SegmentedControl,
  Skeleton,
  Stack,
  Switch,
  Table,
  TagsInput,
  Text,
  TextInput,
  Title,
  Tooltip,
} from '@mantine/core';
import { notifications } from '@mantine/notifications';
import { IconEdit, IconPlus, IconTrash } from '@tabler/icons-react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useEffect, useId, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useParams } from 'react-router';

import { StorageActions, usePermissions } from '@/auth/usePermissions';
import { ConfirmDialog } from '@/components/iam/common';
import { PermissionGuard } from '@/components/iam/PermissionGuard';
import {
  defaultImageSuffixes,
  defaultImageWidths,
  defaultMediaSuffixes,
  defaultThumbnailInterval,
  imageFormatLabels,
  isImageRule,
  mediaImageFormats,
  mediaRenditions,
  mediaRuleSuffixes,
  validateMediaRule,
  type MediaRuleIssue,
} from '@/lib/mediaRules';
import {
  defaultMediaProtection,
  maxMaxSessions,
  maxPaceFactor,
  maxTokenTtlSeconds,
  unlimitedSessions,
  validateMediaProtection,
  type MediaProtectionIssue,
} from '@/lib/mediaProtection';
import { problemMessage } from '@/lib/problem';
import { storageKeys } from '@/lib/storageKeys';
import {
  isApiError,
  useServices,
  type MediaImageFormat,
  type MediaProtection,
  type MediaRendition,
  type MediaRule,
  type RuleProblem,
} from '@/services';

function ruleProblemsOf(error: unknown): readonly RuleProblem[] {
  return isApiError(error) ? error.ruleErrors : [];
}

interface RuleEditorProps {
  opened: boolean;
  rule: MediaRule | null;
  /** The ids of the other rules (duplicates). */
  otherIds: readonly string[];
  saving: boolean;
  problems: readonly RuleProblem[];
  error: string | undefined;
  onSave: (rule: MediaRule) => void;
  onClose: () => void;
}

/** What a rule derives: video outputs or image variants. */
type RuleKind = 'video' | 'images';

/**
 * Editor of one rule: id, keys selected, then for a video rule its
 * renditions, poster and thumbnails, for an image rule the formats and
 * widths of the variants.
 */
function RuleEditor({
  opened,
  rule,
  otherIds,
  saving,
  problems,
  error,
  onSave,
  onClose,
}: RuleEditorProps) {
  const { t } = useTranslation();
  const kindLabel = useId();
  const [kind, setKind] = useState<RuleKind>('video');
  const [formats, setFormats] = useState<string[]>([]);
  const [widths, setWidths] = useState<string[]>([]);
  const [id, setId] = useState('');
  const [prefix, setPrefix] = useState('');
  const [suffixes, setSuffixes] = useState<string[]>([]);
  const [renditions, setRenditions] = useState<string[]>([]);
  const [poster, setPoster] = useState(false);
  const [thumbnails, setThumbnails] = useState(false);
  const [interval, setThumbInterval] = useState(defaultThumbnailInterval);
  const [subtitles, setSubtitles] = useState(false);
  const [spoken, setSpoken] = useState('');
  const [translations, setTranslations] = useState<string[]>([]);
  const [submitted, setSubmitted] = useState(false);

  useEffect(() => {
    if (!opened) return;
    setKind(rule !== null && isImageRule(rule) ? 'images' : 'video');
    setFormats(rule?.images?.formats ?? [...mediaImageFormats]);
    setWidths((rule?.images?.widths ?? defaultImageWidths).map(String));
    setId(rule?.id ?? '');
    setPrefix(rule?.prefix ?? '');
    setSuffixes(rule?.suffixes ?? []);
    setRenditions(rule?.renditions ?? (rule === null ? ['720p', '480p'] : []));
    setPoster(rule?.poster ?? rule === null);
    setThumbnails(rule?.thumbnails !== undefined);
    setThumbInterval(rule?.thumbnails?.interval ?? defaultThumbnailInterval);
    setSubtitles(rule?.subtitles !== undefined);
    setSpoken(rule?.subtitles?.source ?? '');
    setTranslations(rule?.subtitles?.languages ?? []);
    setSubmitted(false);
  }, [opened, rule]);

  const images = kind === 'images';
  const draft: MediaRule = {
    id: id.trim(),
    ...(prefix !== '' ? { prefix } : {}),
    ...(suffixes.length > 0 ? { suffixes } : {}),
    ...(images
      ? {
          images: {
            ...(formats.length > 0 ? { formats: formats as MediaImageFormat[] } : {}),
            ...(widths.length > 0 ? { widths: widths.map((w) => Number(w.trim())) } : {}),
          },
        }
      : {
          ...(renditions.length > 0 ? { renditions: renditions as MediaRendition[] } : {}),
          ...(poster ? { poster: true } : {}),
          ...(thumbnails ? { thumbnails: { interval: interval.trim() } } : {}),
          ...(subtitles
            ? {
                subtitles: {
                  ...(spoken.trim() !== '' ? { source: spoken.trim() } : {}),
                  ...(translations.length > 0 ? { languages: translations } : {}),
                },
              }
            : {}),
        }),
  };
  const kindSuffixes = images ? defaultImageSuffixes : defaultMediaSuffixes;
  const issues = validateMediaRule(draft, otherIds);
  const issue = (kind: readonly MediaRuleIssue[]) => {
    const found = submitted ? issues.find((i) => kind.includes(i)) : undefined;
    return found === undefined ? undefined : t(`bucketMedia.issues.${found}`);
  };
  const nothing = issue(['nothing', 'imageVideoOutputs']);

  return (
    <Modal
      opened={opened}
      onClose={onClose}
      title={rule === null ? t('bucketMedia.addTitle') : t('bucketMedia.editTitle')}
      size="lg"
      centered
    >
      <form
        noValidate
        onSubmit={(event) => {
          event.preventDefault();
          setSubmitted(true);
          if (issues.length === 0) onSave(draft);
        }}
      >
        <Stack gap="md">
          <div>
            <Text size="sm" fw={500} id={kindLabel} mb={4}>
              {t('bucketMedia.kind')}
            </Text>
            <SegmentedControl
              aria-labelledby={kindLabel}
              value={kind}
              onChange={(value) => {
                setKind(value === 'images' ? 'images' : 'video');
              }}
              data={[
                { value: 'video', label: t('bucketMedia.kinds.video') },
                { value: 'images', label: t('bucketMedia.kinds.images') },
              ]}
            />
          </div>
          <TextInput
            label={t('bucketMedia.id')}
            description={t('bucketMedia.idHint')}
            placeholder={images ? 'images' : 'videos'}
            required
            value={id}
            error={issue(['id', 'idDuplicate'])}
            onChange={(event) => {
              setId(event.currentTarget.value);
            }}
          />
          <TextInput
            label={t('bucketMedia.prefix')}
            description={t('bucketMedia.prefixHint')}
            placeholder={images ? 'photos/' : 'videos/'}
            value={prefix}
            error={issue(['prefixTooLong', 'prefixReserved'])}
            onChange={(event) => {
              setPrefix(event.currentTarget.value);
            }}
          />
          <TagsInput
            label={t('bucketMedia.suffixes')}
            description={t('bucketMedia.suffixesHint', {
              suffixes: kindSuffixes.join(', '),
            })}
            placeholder={kindSuffixes.join(' ')}
            value={suffixes}
            error={issue(['suffix'])}
            onChange={setSuffixes}
          />
          {images ? (
            <>
              <MultiSelect
                label={t('bucketMedia.formats')}
                description={t('bucketMedia.formatsHint')}
                data={mediaImageFormats.map((f) => ({ value: f, label: imageFormatLabels[f] }))}
                value={formats}
                error={issue(['imageFormat'])}
                onChange={setFormats}
              />
              <TagsInput
                label={t('bucketMedia.widths')}
                description={t('bucketMedia.widthsHint', {
                  widths: defaultImageWidths.join(', '),
                })}
                placeholder={defaultImageWidths.join(' ')}
                value={widths}
                error={issue(['imageWidth', 'imageWidths'])}
                splitChars={[',', ' ']}
                onChange={setWidths}
              />
            </>
          ) : (
            <>
              <MultiSelect
                label={t('bucketMedia.renditions')}
                description={t('bucketMedia.renditionsHint')}
                data={mediaRenditions.map((r) => ({ value: r, label: r }))}
                value={renditions}
                onChange={setRenditions}
              />
              <Checkbox
                label={t('bucketMedia.poster')}
                description={t('bucketMedia.posterHint')}
                checked={poster}
                onChange={(event) => {
                  setPoster(event.currentTarget.checked);
                }}
              />
              <Switch
                label={t('bucketMedia.thumbnails')}
                description={t('bucketMedia.thumbnailsHint')}
                checked={thumbnails}
                onChange={(event) => {
                  setThumbnails(event.currentTarget.checked);
                }}
              />
              {thumbnails ? (
                <TextInput
                  label={t('bucketMedia.interval')}
                  description={t('bucketMedia.intervalHint')}
                  placeholder={defaultThumbnailInterval}
                  value={interval}
                  error={issue(['interval'])}
                  onChange={(event) => {
                    setThumbInterval(event.currentTarget.value);
                  }}
                />
              ) : null}
              <Switch
                label={t('bucketMedia.subtitles')}
                description={t('bucketMedia.subtitlesHint')}
                checked={subtitles}
                onChange={(event) => {
                  setSubtitles(event.currentTarget.checked);
                }}
              />
              {subtitles ? (
                <>
                  <TextInput
                    label={t('bucketMedia.spoken')}
                    description={t('bucketMedia.spokenHint')}
                    placeholder={t('bucketMedia.spokenPlaceholder')}
                    value={spoken}
                    error={issue(['subtitleLanguage'])}
                    onChange={(event) => {
                      setSpoken(event.currentTarget.value);
                    }}
                  />
                  <TagsInput
                    label={t('bucketMedia.translations')}
                    description={t('bucketMedia.translationsHint')}
                    placeholder="en, de, pt-BR"
                    value={translations}
                    error={issue(['subtitleLanguages'])}
                    splitChars={[',', ' ']}
                    onChange={setTranslations}
                  />
                </>
              ) : null}
            </>
          )}
          {nothing !== undefined ? (
            <Text size="sm" c="red" role="alert">
              {nothing}
            </Text>
          ) : null}
          {problems.length > 0 || error !== undefined ? (
            <Alert color="red" variant="light" role="alert" title={t('bucketMedia.serverRefused')}>
              <Stack gap={2}>
                {problems.map((p) => (
                  <Text key={`${p.code}-${p.message}`} size="sm">
                    {p.message} ({p.code})
                  </Text>
                ))}
                {error !== undefined ? <Text size="sm">{error}</Text> : null}
              </Stack>
            </Alert>
          ) : null}
          <Group justify="flex-end" gap="xs">
            <Button variant="default" onClick={onClose}>
              {t('common.cancel')}
            </Button>
            <Button type="submit" loading={saving}>
              {t('bucketMedia.save')}
            </Button>
          </Group>
        </Stack>
      </form>
    </Modal>
  );
}

/** What a rule derives from the objects it selects, as badges. */
function RuleOutputs({ rule }: { rule: MediaRule }) {
  const { t } = useTranslation();
  if (rule.images !== undefined) {
    const formats = rule.images.formats?.length ? rule.images.formats : mediaImageFormats;
    const widths = rule.images.widths?.length ? rule.images.widths : defaultImageWidths;
    return (
      <Group gap={4}>
        {formats.map((f) => (
          <Badge key={f} variant="light" color="teal" tt="none">
            {imageFormatLabels[f]}
          </Badge>
        ))}
        <Badge variant="outline" color="gray" tt="none">
          {t('bucketMedia.widthsBadge', { widths: widths.join(', ') })}
        </Badge>
      </Group>
    );
  }
  return (
    <Group gap={4}>
      {(rule.renditions ?? []).map((r) => (
        <Badge key={r} variant="outline" color="gray" tt="none">
          {r}
        </Badge>
      ))}
      {rule.poster === true ? (
        <Badge variant="light" tt="none">
          {t('bucketMedia.posterBadge')}
        </Badge>
      ) : null}
      {rule.thumbnails !== undefined ? (
        <Badge variant="light" color="grape" tt="none">
          {t('bucketMedia.thumbnailsBadge', {
            interval: rule.thumbnails.interval ?? defaultThumbnailInterval,
          })}
        </Badge>
      ) : null}
      {rule.subtitles !== undefined ? (
        <Badge variant="light" color="cyan" tt="none">
          {t('bucketMedia.subtitlesBadge', {
            languages: [
              rule.subtitles.source ?? t('bucketMedia.detected'),
              ...(rule.subtitles.languages ?? []),
            ].join(', '),
          })}
        </Badge>
      ) : null}
    </Group>
  );
}

/** A number field as a number: 0 (the default) when empty. */
const numberOf = (value: string | number) => (value === '' ? 0 : Number(value));

type PlaybackMode = NonNullable<MediaProtection['mode']>;
type LegacyDevices = NonNullable<MediaProtection['legacyDevices']>;

/**
 * The download protection of the videos of a bucket: encrypted segments
 * whose keys are served only to playback tokens and authorized users, at
 * playback pace, to a bounded number of streams per viewer; the original
 * is not downloadable without nexo:DownloadProtectedMedia.
 */
function ProtectionSection({ bucket, canEdit }: { bucket: string; canEdit: boolean }) {
  const { t } = useTranslation();
  const { media } = useServices();
  const queryClient = useQueryClient();
  const key = storageKeys.bucketSetting(bucket, 'mediaProtection');
  const modeLabel = useId();
  const query = useQuery({ queryKey: key, queryFn: () => media.getProtection(bucket) });
  const [enabled, setEnabled] = useState(false);
  const [prefix, setPrefix] = useState('');
  const [suffixes, setSuffixes] = useState<string[]>([]);
  const [ttlMinutes, setTtlMinutes] = useState<string | number>('');
  const [unlimited, setUnlimited] = useState(false);
  const [sessions, setSessions] = useState<string | number>('');
  const [pace, setPace] = useState<string | number>('');
  const [watermark, setWatermark] = useState(true);
  const [origins, setOrigins] = useState<string[]>([]);
  const [shareLinks, setShareLinks] = useState(false);
  const [mode, setMode] = useState<PlaybackMode>('standard');
  const [legacyDevices, setLegacyDevices] = useState<LegacyDevices>('refuse');
  const [forensic, setForensic] = useState(false);
  const [submitted, setSubmitted] = useState(false);

  useEffect(() => {
    const p = query.data ?? defaultMediaProtection();
    const fallback = defaultMediaProtection();
    setEnabled(p.enabled);
    setPrefix(p.prefix ?? '');
    setSuffixes(p.suffixes ?? []);
    setTtlMinutes(Math.round((p.tokenTtlSeconds ?? fallback.tokenTtlSeconds ?? 0) / 60));
    setUnlimited(p.maxSessions === unlimitedSessions);
    setSessions(
      p.maxSessions === unlimitedSessions
        ? (fallback.maxSessions ?? '')
        : (p.maxSessions ?? fallback.maxSessions ?? ''),
    );
    setPace(p.paceFactor ?? fallback.paceFactor ?? '');
    setWatermark(p.watermark);
    setOrigins(p.allowedOrigins);
    setShareLinks(p.shareLinks);
    setMode(p.mode ?? 'standard');
    setLegacyDevices(p.legacyDevices ?? 'refuse');
    setForensic(p.forensic);
    setSubmitted(false);
  }, [query.data]);

  const save = useMutation({
    mutationFn: (next: MediaProtection) => media.setProtection(bucket, next),
    onSuccess: (stored) => {
      queryClient.setQueryData(key, stored);
      notifications.show({ color: 'green', message: t('bucketMedia.protection.saved') });
    },
  });

  const draft: MediaProtection = {
    enabled,
    ...(prefix !== '' ? { prefix } : {}),
    ...(suffixes.length > 0 ? { suffixes } : {}),
    tokenTtlSeconds: numberOf(ttlMinutes) * 60,
    maxSessions: unlimited ? unlimitedSessions : numberOf(sessions),
    paceFactor: numberOf(pace),
    watermark,
    allowedOrigins: origins,
    shareLinks,
    mode,
    legacyDevices,
    forensic,
  };
  const issues = validateMediaProtection(draft);
  const issue = (kind: readonly MediaProtectionIssue[]) => {
    const found = submitted ? issues.find((i) => kind.includes(i)) : undefined;
    return found === undefined ? undefined : t(`bucketMedia.protection.issues.${found}`);
  };
  const disabled = !canEdit || query.isPending;

  return (
    <Paper
      withBorder
      radius="md"
      p="md"
      component="section"
      aria-label={t('bucketMedia.protection.title')}
    >
      <form
        noValidate
        onSubmit={(event) => {
          event.preventDefault();
          setSubmitted(true);
          if (issues.length === 0) save.mutate(draft);
        }}
      >
        <Stack gap="md">
          <Stack gap={2}>
            <Title order={4}>{t('bucketMedia.protection.title')}</Title>
            <Text size="sm" c="dimmed">
              {t('bucketMedia.protection.description')}
            </Text>
          </Stack>
          {query.isError ? (
            <Alert color="red" variant="light" role="alert">
              {problemMessage(t, query.error)}
            </Alert>
          ) : query.isPending ? (
            <Skeleton height={60} />
          ) : (
            <>
              <Switch
                label={t('bucketMedia.protection.enabled')}
                description={t('bucketMedia.protection.enabledHint')}
                checked={enabled}
                disabled={disabled}
                onChange={(event) => {
                  setEnabled(event.currentTarget.checked);
                }}
              />
              {enabled ? (
                <>
                  <Stack gap={4}>
                    <Text size="sm" fw={500} id={modeLabel}>
                      {t('bucketMedia.protection.mode')}
                    </Text>
                    <SegmentedControl
                      aria-labelledby={modeLabel}
                      value={mode}
                      disabled={disabled}
                      data={[
                        { value: 'standard', label: t('bucketMedia.protection.modeStandard') },
                        { value: 'advanced', label: t('bucketMedia.protection.modeAdvanced') },
                      ]}
                      onChange={(value) => {
                        setMode(value as PlaybackMode);
                      }}
                    />
                    <Text size="xs" c="dimmed">
                      {mode === 'advanced'
                        ? t('bucketMedia.protection.modeAdvancedHint')
                        : t('bucketMedia.protection.modeStandardHint')}
                    </Text>
                  </Stack>
                  {mode === 'advanced' ? (
                    <Switch
                      label={t('bucketMedia.protection.legacyDevices')}
                      description={t('bucketMedia.protection.legacyDevicesHint')}
                      checked={legacyDevices === 'standard'}
                      disabled={disabled}
                      onChange={(event) => {
                        setLegacyDevices(event.currentTarget.checked ? 'standard' : 'refuse');
                      }}
                    />
                  ) : null}
                  <Switch
                    label={t('bucketMedia.protection.forensic')}
                    description={t('bucketMedia.protection.forensicHint')}
                    checked={forensic}
                    disabled={disabled}
                    onChange={(event) => {
                      setForensic(event.currentTarget.checked);
                    }}
                  />
                  <TextInput
                    label={t('bucketMedia.prefix')}
                    description={t('bucketMedia.protection.prefixHint')}
                    placeholder="courses/"
                    value={prefix}
                    disabled={disabled}
                    error={issue(['prefixTooLong', 'prefixReserved'])}
                    onChange={(event) => {
                      setPrefix(event.currentTarget.value);
                    }}
                  />
                  <Group align="flex-end" gap="md" wrap="wrap">
                    <NumberInput
                      label={t('bucketMedia.protection.sessions')}
                      description={t('bucketMedia.protection.sessionsHint')}
                      min={1}
                      max={maxMaxSessions}
                      allowDecimal={false}
                      w={260}
                      value={sessions}
                      disabled={disabled || unlimited}
                      error={issue(['maxSessions'])}
                      onChange={setSessions}
                    />
                    <Checkbox
                      label={t('bucketMedia.protection.unlimited')}
                      checked={unlimited}
                      disabled={disabled}
                      mb={8}
                      onChange={(event) => {
                        setUnlimited(event.currentTarget.checked);
                      }}
                    />
                  </Group>
                  <TagsInput
                    label={t('bucketMedia.protection.origins')}
                    description={t('bucketMedia.protection.originsHint')}
                    placeholder="https://learn.example.com"
                    value={origins}
                    disabled={disabled}
                    error={issue(['origins', 'origin'])}
                    splitChars={[',', ' ']}
                    onChange={setOrigins}
                  />
                  <Switch
                    label={t('bucketMedia.protection.watermark')}
                    description={t('bucketMedia.protection.watermarkHint')}
                    checked={watermark}
                    disabled={disabled}
                    onChange={(event) => {
                      setWatermark(event.currentTarget.checked);
                    }}
                  />
                  <Switch
                    label={t('bucketMedia.protection.shareLinks')}
                    description={t('bucketMedia.protection.shareLinksHint')}
                    checked={shareLinks}
                    disabled={disabled}
                    onChange={(event) => {
                      setShareLinks(event.currentTarget.checked);
                    }}
                  />
                  <Accordion variant="contained">
                    <Accordion.Item value="advanced">
                      <Accordion.Control>{t('bucketMedia.protection.advanced')}</Accordion.Control>
                      <Accordion.Panel>
                        <Stack gap="md">
                          <NumberInput
                            label={t('bucketMedia.protection.ttl')}
                            description={t('bucketMedia.protection.ttlHint')}
                            min={1}
                            max={maxTokenTtlSeconds / 60}
                            allowDecimal={false}
                            value={ttlMinutes}
                            disabled={disabled}
                            error={issue(['tokenTtl'])}
                            onChange={setTtlMinutes}
                          />
                          <NumberInput
                            label={t('bucketMedia.protection.pace')}
                            description={t('bucketMedia.protection.paceHint')}
                            min={1}
                            max={maxPaceFactor}
                            allowDecimal={false}
                            value={pace}
                            disabled={disabled}
                            error={issue(['paceFactor'])}
                            onChange={setPace}
                          />
                          <TagsInput
                            label={t('bucketMedia.suffixes')}
                            description={t('bucketMedia.suffixesHint', {
                              suffixes: defaultMediaSuffixes.join(', '),
                            })}
                            placeholder={defaultMediaSuffixes.join(' ')}
                            value={suffixes}
                            disabled={disabled}
                            error={issue(['suffix'])}
                            onChange={setSuffixes}
                          />
                        </Stack>
                      </Accordion.Panel>
                    </Accordion.Item>
                  </Accordion>
                </>
              ) : null}
              {save.isError ? (
                <Alert
                  color="red"
                  variant="light"
                  role="alert"
                  title={t('bucketMedia.protection.refused')}
                >
                  {problemMessage(t, save.error)}
                </Alert>
              ) : null}
              <Group justify="flex-end">
                <PermissionGuard allowed={canEdit} action={StorageActions.setMedia}>
                  {(guarded) => (
                    <Button type="submit" loading={save.isPending} disabled={guarded || disabled}>
                      {t('bucketMedia.protection.save')}
                    </Button>
                  )}
                </PermissionGuard>
              </Group>
            </>
          )}
        </Stack>
      </form>
    </Paper>
  );
}

/**
 * The media rules of a bucket: what the media worker derives from its
 * videos and images.
 */
export function BucketMediaTab() {
  const { t } = useTranslation();
  const { bucket = '' } = useParams();
  const { media } = useServices();
  const queryClient = useQueryClient();
  const perms = usePermissions({ bucket });
  const canRead = !perms.ready || perms.can(StorageActions.getMedia);
  const canEdit = perms.can(StorageActions.setMedia);
  const [editing, setEditing] = useState<number | null>(null);
  const [deleting, setDeleting] = useState<number | null>(null);
  const key = storageKeys.bucketSetting(bucket, 'media');
  const query = useQuery({ queryKey: key, queryFn: () => media.get(bucket), enabled: canRead });
  const rules = query.data?.rules ?? [];

  const save = useMutation({
    mutationFn: async (next: MediaRule[]) => {
      if (next.length === 0) {
        await media.remove(bucket);
        return { rules: [] };
      }
      return media.set(bucket, { rules: next });
    },
    onSuccess: (stored) => {
      queryClient.setQueryData(key, stored);
      setEditing(null);
      setDeleting(null);
      notifications.show({ color: 'green', message: t('bucketMedia.saved') });
    },
  });
  const saveWith = (index: number, rule: MediaRule | null) => {
    save.mutate(
      rule === null
        ? rules.filter((_, i) => i !== index)
        : index < 0
          ? [...rules, rule]
          : rules.map((r, i) => (i === index ? rule : r)),
    );
  };
  const editedIndex = editing === null ? -1 : editing < 0 ? rules.length : editing;
  const problems = ruleProblemsOf(save.error).filter(
    (p) => p.rule === editedIndex + 1 || p.rule === 0,
  );

  if (!canRead || (query.isError && isApiError(query.error) && query.error.isForbidden)) {
    return (
      <Alert color="red" variant="light" role="alert">
        {t('bucketMedia.unavailable')}
      </Alert>
    );
  }
  if (query.isError) {
    return (
      <Alert color="red" variant="light" role="alert">
        {problemMessage(t, query.error)}
      </Alert>
    );
  }

  return (
    <Stack gap="md">
      <Paper withBorder radius="md" p="md" component="section" aria-label={t('bucketMedia.title')}>
        <Stack gap="md">
          <Group justify="space-between" align="flex-start" wrap="wrap" gap="sm">
            <Stack gap={2}>
              <Title order={4}>{t('bucketMedia.title')}</Title>
              <Text size="sm" c="dimmed">
                {t('bucketMedia.description')}
              </Text>
            </Stack>
            <PermissionGuard allowed={canEdit} action={StorageActions.setMedia}>
              {(disabled) => (
                <Button
                  leftSection={<IconPlus size={16} />}
                  disabled={disabled || query.isPending}
                  onClick={() => {
                    save.reset();
                    setEditing(-1);
                  }}
                >
                  {t('bucketMedia.add')}
                </Button>
              )}
            </PermissionGuard>
          </Group>
          {query.isPending ? (
            <Skeleton height={100} />
          ) : rules.length === 0 ? (
            <Text size="sm" c="dimmed">
              {t('bucketMedia.empty')}
            </Text>
          ) : (
            <Table.ScrollContainer minWidth={640}>
              <Table aria-label={t('bucketMedia.rules')} verticalSpacing="sm">
                <Table.Thead>
                  <Table.Tr>
                    <Table.Th>{t('bucketMedia.columns.id')}</Table.Th>
                    <Table.Th>{t('bucketMedia.columns.objects')}</Table.Th>
                    <Table.Th>{t('bucketMedia.columns.outputs')}</Table.Th>
                    <Table.Th w={90}>
                      <Text size="sm" fw={700} style={{ visibility: 'hidden' }}>
                        {t('common.actions')}
                      </Text>
                    </Table.Th>
                  </Table.Tr>
                </Table.Thead>
                <Table.Tbody>
                  {rules.map((rule, index) => (
                    <Table.Tr key={rule.id}>
                      <Table.Td>
                        <Stack gap={0}>
                          <Text size="sm" ff="monospace" style={{ overflowWrap: 'anywhere' }}>
                            {rule.id}
                          </Text>
                          <Text size="xs" c="dimmed">
                            {t(`bucketMedia.kinds.${isImageRule(rule) ? 'images' : 'video'}`)}
                          </Text>
                        </Stack>
                      </Table.Td>
                      <Table.Td>
                        <Stack gap={0}>
                          <Text size="sm" style={{ overflowWrap: 'anywhere' }}>
                            {rule.prefix
                              ? t('bucketMedia.prefixIs', { prefix: rule.prefix })
                              : t('bucketMedia.wholeBucket')}
                          </Text>
                          <Text size="xs" c="dimmed">
                            {mediaRuleSuffixes(rule).join(', ')}
                          </Text>
                        </Stack>
                      </Table.Td>
                      <Table.Td>
                        <RuleOutputs rule={rule} />
                      </Table.Td>
                      <Table.Td>
                        {canEdit ? (
                          <Group gap={4} wrap="nowrap" justify="flex-end">
                            <Tooltip label={t('bucketMedia.edit', { id: rule.id })}>
                              <ActionIcon
                                variant="subtle"
                                aria-label={t('bucketMedia.edit', { id: rule.id })}
                                onClick={() => {
                                  save.reset();
                                  setEditing(index);
                                }}
                              >
                                <IconEdit size={16} />
                              </ActionIcon>
                            </Tooltip>
                            <Tooltip label={t('bucketMedia.remove', { id: rule.id })}>
                              <ActionIcon
                                variant="subtle"
                                color="red"
                                aria-label={t('bucketMedia.remove', { id: rule.id })}
                                onClick={() => {
                                  save.reset();
                                  setDeleting(index);
                                }}
                              >
                                <IconTrash size={16} />
                              </ActionIcon>
                            </Tooltip>
                          </Group>
                        ) : null}
                      </Table.Td>
                    </Table.Tr>
                  ))}
                </Table.Tbody>
              </Table>
            </Table.ScrollContainer>
          )}
          <Text size="xs" c="dimmed">
            {t('bucketMedia.firstMatch')}
          </Text>
        </Stack>
      </Paper>

      <ProtectionSection bucket={bucket} canEdit={canEdit} />

      <RuleEditor
        opened={editing !== null}
        rule={editing === null || editing < 0 ? null : (rules[editing] ?? null)}
        otherIds={rules.filter((_, i) => i !== editing).map((r) => r.id)}
        saving={save.isPending}
        problems={editing !== null ? problems : []}
        error={
          editing !== null && save.isError && ruleProblemsOf(save.error).length === 0
            ? problemMessage(t, save.error)
            : undefined
        }
        onSave={(rule) => {
          if (editing !== null) saveWith(editing, rule);
        }}
        onClose={() => {
          setEditing(null);
          save.reset();
        }}
      />
      <ConfirmDialog
        opened={deleting !== null}
        title={t('bucketMedia.deleteTitle')}
        message={t('bucketMedia.deleteConfirm', {
          id: deleting === null ? '' : (rules[deleting]?.id ?? ''),
        })}
        confirmLabel={t('common.delete')}
        loading={save.isPending}
        error={deleting !== null && save.isError ? save.error : undefined}
        onCancel={() => {
          setDeleting(null);
          save.reset();
        }}
        onConfirm={() => {
          if (deleting !== null) saveWith(deleting, null);
        }}
      />
    </Stack>
  );
}
