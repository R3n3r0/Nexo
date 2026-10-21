import {
  Alert,
  Autocomplete,
  Button,
  Checkbox,
  Group,
  Modal,
  NumberInput,
  Select,
  SimpleGrid,
  Stack,
  Switch,
  TagsInput,
  Text,
  TextInput,
} from '@mantine/core';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';

import { PatternField } from '@/components/accessRules/PatternField';
import { patternIssue } from '@/lib/keyPattern';
import { KeyValueEditor } from '@/components/common/KeyValueEditor';
import { accessModes, behaviorMethods } from '@/lib/distributions';
import type { DistributionAccess, DistributionBehavior, DistributionCors } from '@/services';

export interface BehaviorEditorProps {
  /** The behavior edited. */
  behavior: DistributionBehavior;
  /** The default behavior: no path pattern, nothing to inherit. */
  isDefault: boolean;
  isNew: boolean;
  /** Key groups offered to the signed behaviors. */
  keyGroups: readonly string[];
  website: boolean;
  readOnly?: boolean;
  onSave: (behavior: DistributionBehavior) => void;
  onClose: () => void;
}

/** Cache-Control values offered by the field. */
const cacheControlPresets = [
  'public, max-age=300',
  'public, max-age=3600',
  'public, max-age=31536000, immutable',
  'no-cache',
  'no-store',
];

/** The behavior without its empty members. */
function compact(b: DistributionBehavior, isDefault: boolean): DistributionBehavior {
  const out: DistributionBehavior = {};
  if (!isDefault && b.pathPattern !== undefined) out.pathPattern = b.pathPattern;
  if (b.access !== undefined) out.access = b.access;
  if (b.allowedMethods !== undefined && b.allowedMethods.length > 0)
    out.allowedMethods = b.allowedMethods;
  if ((b.indexDocument ?? '') !== '') out.indexDocument = b.indexDocument;
  if ((b.spaFallback ?? '') !== '') out.spaFallback = b.spaFallback;
  if ((b.cache?.cacheControl ?? '') !== '') out.cache = { cacheControl: b.cache?.cacheControl };
  if (b.access === 'signed' && (b.signing?.keyGroup ?? '') !== '') out.signing = b.signing;
  if (b.responseHeaders !== undefined && Object.keys(b.responseHeaders).length > 0) {
    out.responseHeaders = b.responseHeaders;
  }
  if (b.cors !== undefined && b.cors.allowedOrigins.length > 0) out.cors = b.cors;
  // The default behavior states the option only when it is on.
  if (b.images !== undefined && (!isDefault || b.images.negotiate)) out.images = b.images;
  if (b.video !== undefined && (!isDefault || b.video.hls)) out.video = b.video;
  return out;
}

/** The value of the images select: inherited (not the default), on or off. */
type ImagesChoice = 'inherit' | 'negotiate' | 'original';

function imagesChoice(b: DistributionBehavior, isDefault: boolean): ImagesChoice {
  if (b.images === undefined) return isDefault ? 'original' : 'inherit';
  return b.images.negotiate ? 'negotiate' : 'original';
}

/** The value of the video select: inherited (not the default), on or off. */
type VideoChoice = 'inherit' | 'hls' | 'off';

function videoChoice(b: DistributionBehavior, isDefault: boolean): VideoChoice {
  if (b.video === undefined) return isDefault ? 'off' : 'inherit';
  return b.video.hls ? 'hls' : 'off';
}

/**
 * D-DST-03: editor of a behavior of a distribution: the paths (prefix,
 * glob or regex), the access (public, signed with a key group,
 * authenticated, deny), the methods, the index document and SPA fallback,
 * the Cache-Control sent to the browsers, the images in every format on
 * their own URL and the HLS streaming of the videos (website mode),
 * response headers and CORS. An unset field
 * inherits from the default behavior.
 */
export function BehaviorEditor({
  behavior,
  isDefault,
  isNew,
  keyGroups,
  website,
  readOnly = false,
  onSave,
  onClose,
}: BehaviorEditorProps) {
  const { t } = useTranslation();
  const [draft, setDraft] = useState<DistributionBehavior>(behavior);
  const [submitted, setSubmitted] = useState(false);
  const inheritMethods = draft.allowedMethods === undefined;
  const cors: DistributionCors | undefined = draft.cors;
  const patternMissing =
    !isDefault &&
    (draft.pathPattern === undefined ||
      (draft.pathPattern.type !== 'prefix' && draft.pathPattern.pattern === ''));
  const patternBad = !isDefault && patternIssue(draft.pathPattern) !== undefined;
  const keyGroupMissing = draft.access === 'signed' && (draft.signing?.keyGroup ?? '') === '';
  const invalid = patternMissing || patternBad || keyGroupMissing;

  const set = (patch: Partial<DistributionBehavior>) => {
    setDraft((d) => ({ ...d, ...patch }));
  };

  const accessData = [
    ...(isDefault ? [] : [{ value: 'inherit', label: t('distributions.behaviors.inherit') }]),
    ...accessModes.map((a) => ({ value: a, label: t(`distributions.access.${a}`) })),
  ];

  return (
    <Modal
      opened
      onClose={onClose}
      size="lg"
      title={
        isDefault
          ? t('distributions.behaviors.defaultTitle')
          : isNew
            ? t('distributions.behaviors.newTitle')
            : t('distributions.behaviors.editTitle')
      }
    >
      <form
        noValidate
        onSubmit={(event) => {
          event.preventDefault();
          setSubmitted(true);
          if (!invalid && !readOnly) onSave(compact(draft, isDefault));
        }}
      >
        <Stack gap="md">
          {!isDefault ? (
            <PatternField
              label={t('distributions.behaviors.pathPattern')}
              value={draft.pathPattern ?? { type: 'glob', pattern: '' }}
              disabled={readOnly}
              placeholders={{
                prefix: '/internal/',
                glob: '/assets/**',
                regex: '^/downloads/.*\\.zip$',
              }}
              onChange={(pathPattern) => {
                set({ pathPattern });
              }}
            />
          ) : null}
          {submitted && patternMissing ? (
            <Text size="sm" c="red" role="alert">
              {t('distributions.behaviors.patternRequired')}
            </Text>
          ) : null}
          <SimpleGrid cols={{ base: 1, sm: 2 }} spacing="sm">
            <Select
              label={t('distributions.fields.access')}
              data={accessData}
              value={draft.access ?? (isDefault ? 'public' : 'inherit')}
              allowDeselect={false}
              disabled={readOnly}
              onChange={(value) => {
                setDraft((d) => {
                  const { access: _a, ...rest } = d;
                  return value === 'inherit' || value === null
                    ? rest
                    : { ...rest, access: value as DistributionAccess };
                });
              }}
            />
            {draft.access === 'signed' ? (
              <Select
                label={t('distributions.fields.keyGroup')}
                placeholder={t('distributions.behaviors.chooseKeyGroup')}
                data={[...keyGroups]}
                value={draft.signing?.keyGroup ?? null}
                disabled={readOnly}
                error={
                  submitted && keyGroupMissing
                    ? t('distributions.behaviors.keyGroupRequired')
                    : undefined
                }
                onChange={(value) => {
                  set({ signing: { keyGroup: value ?? '' } });
                }}
              />
            ) : null}
          </SimpleGrid>
          <Text size="xs" c="dimmed">
            {t(`distributions.accessHelp.${draft.access ?? 'public'}`)}
          </Text>
          <Stack gap={4}>
            {!isDefault ? (
              <Switch
                label={t('distributions.behaviors.inheritMethods')}
                checked={inheritMethods}
                disabled={readOnly}
                onChange={(event) => {
                  const checked = event.currentTarget.checked;
                  setDraft((d) => {
                    const { allowedMethods: _m, ...rest } = d;
                    return checked ? rest : { ...rest, allowedMethods: ['GET', 'HEAD', 'OPTIONS'] };
                  });
                }}
              />
            ) : null}
            {!inheritMethods || isDefault ? (
              <Checkbox.Group
                label={t('distributions.fields.methods')}
                description={t('distributions.behaviors.methodsHint')}
                value={draft.allowedMethods ?? ['GET', 'HEAD', 'OPTIONS']}
                onChange={(allowedMethods) => {
                  set({ allowedMethods });
                }}
              >
                <Group gap="md" mt={6}>
                  {behaviorMethods.map((m) => (
                    <Checkbox
                      key={m}
                      value={m}
                      label={m}
                      disabled={
                        readOnly ||
                        ((m === 'POST' || m === 'PUT') && (!website || draft.access !== 'signed'))
                      }
                    />
                  ))}
                </Group>
              </Checkbox.Group>
            ) : null}
          </Stack>
          {website ? (
            <SimpleGrid cols={{ base: 1, sm: 2 }} spacing="sm">
              <TextInput
                label={t('distributions.fields.indexDocument')}
                placeholder={isDefault ? 'index.html' : t('distributions.behaviors.inherited')}
                value={draft.indexDocument ?? ''}
                disabled={readOnly}
                onChange={(event) => {
                  set({ indexDocument: event.currentTarget.value });
                }}
              />
              <TextInput
                label={t('distributions.fields.spaFallback')}
                description={t('distributions.behaviors.spaHint')}
                placeholder="/index.html"
                value={draft.spaFallback ?? ''}
                disabled={readOnly}
                onChange={(event) => {
                  set({ spaFallback: event.currentTarget.value });
                }}
              />
            </SimpleGrid>
          ) : null}
          {website ? (
            <Select
              label={t('distributions.behaviors.images')}
              description={t('distributions.behaviors.imagesHint')}
              data={[
                ...(isDefault
                  ? []
                  : [{ value: 'inherit', label: t('distributions.behaviors.inherit') }]),
                { value: 'negotiate', label: t('distributions.behaviors.imagesOn') },
                { value: 'original', label: t('distributions.behaviors.imagesOff') },
              ]}
              value={imagesChoice(draft, isDefault)}
              allowDeselect={false}
              disabled={readOnly}
              onChange={(value) => {
                setDraft((d) => {
                  const { images: _i, ...rest } = d;
                  return value === 'inherit' || value === null
                    ? rest
                    : { ...rest, images: { negotiate: value === 'negotiate' } };
                });
              }}
            />
          ) : null}
          {website ? (
            <Select
              label={t('distributions.behaviors.video')}
              description={t('distributions.behaviors.videoHint')}
              data={[
                ...(isDefault
                  ? []
                  : [{ value: 'inherit', label: t('distributions.behaviors.inherit') }]),
                { value: 'hls', label: t('distributions.behaviors.videoOn') },
                { value: 'off', label: t('distributions.behaviors.videoOff') },
              ]}
              value={videoChoice(draft, isDefault)}
              allowDeselect={false}
              disabled={readOnly}
              onChange={(value) => {
                setDraft((d) => {
                  const { video: _v, ...rest } = d;
                  return value === 'inherit' || value === null
                    ? rest
                    : { ...rest, video: { hls: value === 'hls' } };
                });
              }}
            />
          ) : null}
          <Autocomplete
            label={t('distributions.fields.cacheControl')}
            description={t('distributions.behaviors.cacheHint')}
            placeholder={isDefault ? 'public, max-age=300' : t('distributions.behaviors.inherited')}
            data={cacheControlPresets}
            value={draft.cache?.cacheControl ?? ''}
            disabled={readOnly}
            onChange={(value) => {
              setDraft((d) => {
                const { cache: _c, ...rest } = d;
                return value === '' ? rest : { ...rest, cache: { cacheControl: value } };
              });
            }}
          />
          <KeyValueEditor
            label={t('distributions.fields.responseHeaders')}
            keyLabel={t('distributions.fields.headerName')}
            valueLabel={t('distributions.fields.headerValue')}
            keyPlaceholder="X-Robots-Tag"
            valuePlaceholder="noindex"
            value={draft.responseHeaders ?? {}}
            disabled={readOnly}
            onChange={(responseHeaders) => {
              set({ responseHeaders });
            }}
          />
          <Stack gap={6}>
            <TagsInput
              label={t('distributions.fields.corsOrigins')}
              description={t('distributions.behaviors.corsHint')}
              placeholder="https://app.example.com"
              value={cors?.allowedOrigins ?? []}
              disabled={readOnly}
              acceptValueOnBlur
              onChange={(allowedOrigins) => {
                setDraft((d) => {
                  const { cors: _c, ...rest } = d;
                  return allowedOrigins.length === 0
                    ? rest
                    : { ...rest, cors: { ...(d.cors ?? {}), allowedOrigins } };
                });
              }}
            />
            {cors !== undefined ? (
              <NumberInput
                label={t('distributions.fields.corsMaxAge')}
                min={0}
                allowDecimal={false}
                value={cors.maxAge ?? ''}
                disabled={readOnly}
                maw={220}
                onChange={(value) => {
                  set({ cors: { ...cors, ...(value === '' ? {} : { maxAge: Number(value) }) } });
                }}
              />
            ) : null}
          </Stack>
          {submitted && invalid ? (
            <Alert color="red" variant="light" role="alert">
              {t('distributions.behaviors.fixIssues')}
            </Alert>
          ) : null}
          <Group justify="flex-end" gap="xs">
            <Button variant="default" onClick={onClose}>
              {readOnly ? t('common.close') : t('common.cancel')}
            </Button>
            {!readOnly ? <Button type="submit">{t('distributions.behaviors.apply')}</Button> : null}
          </Group>
        </Stack>
      </form>
    </Modal>
  );
}
