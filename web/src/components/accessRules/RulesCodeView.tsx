import { Button, Group, Stack } from '@mantine/core';
import { IconDownload } from '@tabler/icons-react';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';

import { CodeEditor } from '@/components/code/CodeEditor';
import type { CodeMarker } from '@/components/code/types';
import { formatRules, parseRules, type DocumentFormat } from '@/lib/accessRules';
import { saveTextFile } from '@/lib/saveFile';
import type { BucketAccessRule } from '@/services';

export interface RulesCodeViewProps {
  format: DocumentFormat;
  bucket: string;
  rules: readonly BucketAccessRule[];
  canEdit: boolean;
  saving: boolean;
  onSave: (rules: BucketAccessRule[]) => void;
}

/**
 * The rules of the bucket as a YAML or JSON document, the one of `nexoctl
 * access export|import`: editable and saved as a whole, downloadable.
 */
export function RulesCodeView({
  format,
  bucket,
  rules,
  canEdit,
  saving,
  onSave,
}: RulesCodeViewProps) {
  const { t } = useTranslation();
  const serialized = formatRules(format, bucket, rules);
  const [draft, setDraft] = useState<string | null>(null);
  const text = draft ?? serialized;
  const parsed = parseRules(format, text);
  const markers: CodeMarker[] =
    parsed.ok || parsed.line === undefined
      ? []
      : [{ line: parsed.line, column: parsed.column ?? 1, message: parsed.error }];
  return (
    <Stack gap="sm">
      <CodeEditor
        label={
          format === 'yaml' ? t('accessRules.code.yamlLabel') : t('accessRules.code.jsonLabel')
        }
        description={t('accessRules.code.hint')}
        language={format === 'json' ? 'json' : 'plaintext'}
        value={text}
        height={360}
        readOnly={!canEdit}
        markers={markers}
        error={!parsed.ok && parsed.line === undefined ? parsed.error : undefined}
        onChange={setDraft}
      />
      <Group justify="space-between" gap="xs">
        <Button
          variant="subtle"
          leftSection={<IconDownload size={16} />}
          onClick={() => {
            saveTextFile(
              `${bucket}-access-rules.${format === 'yaml' ? 'yaml' : 'json'}`,
              serialized,
              format === 'yaml' ? 'application/yaml' : 'application/json',
            );
          }}
        >
          {t('accessRules.code.download')}
        </Button>
        {canEdit ? (
          <Group gap="xs">
            <Button
              variant="default"
              disabled={draft === null}
              onClick={() => {
                setDraft(null);
              }}
            >
              {t('accessRules.code.reset')}
            </Button>
            <Button
              disabled={draft === null || !parsed.ok}
              loading={saving}
              onClick={() => {
                if (parsed.ok) onSave(parsed.rules);
              }}
            >
              {t('accessRules.code.save')}
            </Button>
          </Group>
        ) : null}
      </Group>
    </Stack>
  );
}
