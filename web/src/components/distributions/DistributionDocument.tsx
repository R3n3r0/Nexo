import { Button, Group, SegmentedControl, Stack, Text } from '@mantine/core';
import { IconDownload } from '@tabler/icons-react';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';

import { CodeEditor } from '@/components/code/CodeEditor';
import type { CodeMarker } from '@/components/code/types';
import { editable } from '@/lib/distributions';
import { saveTextFile } from '@/lib/saveFile';
import { parseYaml, toYaml } from '@/lib/yaml';
import type { Distribution } from '@/services';

export interface DistributionDocumentProps {
  distribution: Distribution;
  readOnly: boolean;
  /** Applies an edited document to the draft (saved with the save bar). */
  onApply: (next: Distribution) => void;
}

type Parsed =
  { ok: true; value: Distribution } | { ok: false; error: string; line?: number; column?: number };

function parse(format: 'yaml' | 'json', text: string, id: string): Parsed {
  let value: unknown;
  if (format === 'yaml') {
    const r = parseYaml(text);
    if (!r.ok)
      return { ok: false, error: r.error.message, line: r.error.line, column: r.error.column };
    value = r.value;
  } else {
    try {
      value = JSON.parse(text) as unknown;
    } catch (error) {
      return { ok: false, error: error instanceof Error ? error.message : String(error) };
    }
  }
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    return { ok: false, error: 'the document must be a mapping' };
  }
  const d = value as Partial<Distribution>;
  if (!Array.isArray(d.aliases) || typeof d.origin !== 'object' || d.origin === null) {
    return { ok: false, error: 'aliases and origin are required' };
  }
  if (d.id !== undefined && d.id !== id) {
    return { ok: false, error: `the id cannot change (${id})` };
  }
  return { ok: true, value: { ...(value as Distribution), id } };
}

/**
 * The whole distribution as YAML or JSON (`nexoctl dist export|import`):
 * an edited document is applied to the draft, then saved like the other
 * tabs.
 */
export function DistributionDocument({
  distribution,
  readOnly,
  onApply,
}: DistributionDocumentProps) {
  const { t } = useTranslation();
  const [format, setFormat] = useState<'yaml' | 'json'>('yaml');
  const serialized =
    format === 'yaml'
      ? toYaml(editable(distribution))
      : `${JSON.stringify(editable(distribution), null, 2)}\n`;
  const [text, setText] = useState<string | null>(null);
  const value = text ?? serialized;
  const parsed = parse(format, value, distribution.id);
  const markers: CodeMarker[] =
    !parsed.ok && parsed.line !== undefined
      ? [{ line: parsed.line, column: parsed.column ?? 1, message: parsed.error }]
      : [];
  return (
    <Stack gap="sm">
      <SegmentedControl
        aria-label={t('distributions.document.format')}
        value={format}
        data={[
          { value: 'yaml', label: 'YAML' },
          { value: 'json', label: 'JSON' },
        ]}
        onChange={(next) => {
          setFormat(next === 'json' ? 'json' : 'yaml');
          setText(null);
        }}
        style={{ alignSelf: 'flex-start' }}
      />
      <CodeEditor
        label={t('distributions.document.label')}
        description={t('distributions.document.hint')}
        language={format === 'json' ? 'json' : 'plaintext'}
        value={value}
        height={420}
        readOnly={readOnly}
        markers={markers}
        error={!parsed.ok && parsed.line === undefined ? parsed.error : undefined}
        onChange={setText}
      />
      <Group justify="space-between">
        <Button
          variant="subtle"
          leftSection={<IconDownload size={16} />}
          onClick={() => {
            saveTextFile(
              `${distribution.id}.${format === 'yaml' ? 'yaml' : 'json'}`,
              serialized,
              format === 'yaml' ? 'application/yaml' : 'application/json',
            );
          }}
        >
          {t('distributions.document.download')}
        </Button>
        {!readOnly ? (
          <Group gap="xs">
            {text !== null && parsed.ok ? (
              <Text size="xs" c="dimmed">
                {t('distributions.document.applyHint')}
              </Text>
            ) : null}
            <Button
              disabled={text === null || !parsed.ok}
              onClick={() => {
                if (parsed.ok) {
                  onApply(parsed.value);
                  setText(null);
                }
              }}
            >
              {t('distributions.document.apply')}
            </Button>
          </Group>
        ) : null}
      </Group>
    </Stack>
  );
}
