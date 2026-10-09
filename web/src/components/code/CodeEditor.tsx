import { Anchor, Input, List, Skeleton, Text } from '@mantine/core';
import { Component, lazy, Suspense, useCallback, useId, useRef, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';

import { PlainCodeEditor } from './PlainCodeEditor';
import type { CodeMarker, CodeSchema, EditorImplProps } from './types';

// Monaco is large: load it only when an editor is shown.
const MonacoCodeEditor = lazy(() => import('./MonacoCodeEditor'));

export interface CodeEditorProps {
  /** Visible label of the field. */
  label: string;
  description?: ReactNode;
  value: string;
  onChange: (value: string) => void;
  /** Height in pixels (default 360). */
  height?: number;
  readOnly?: boolean;
  /** Problems to highlight, also listed under the editor. */
  markers?: readonly CodeMarker[];
  language?: 'json' | 'xml' | 'plaintext';
  /** JSON schema for validation and autocompletion. */
  schema?: CodeSchema;
  /** Field-level error shown under the label. */
  error?: ReactNode;
}

const noMarkers: readonly CodeMarker[] = [];

/** Falls back to the plain editor when Monaco cannot be loaded. */
class MonacoBoundary extends Component<
  { fallback: ReactNode; children: ReactNode },
  { failed: boolean }
> {
  override state = { failed: false };

  static getDerivedStateFromError() {
    return { failed: true };
  }

  override componentDidCatch(error: Error) {
    console.warn('Monaco editor unavailable, using the plain editor', error);
  }

  override render() {
    return this.state.failed ? this.props.fallback : this.props.children;
  }
}

/**
 * Code editor field: Monaco (lazy-loaded, JSON schema, markers) with a
 * plain textarea fallback, a label and the list of problems under it. The
 * list makes every marker reachable by keyboard and screen readers, and a
 * click moves the cursor to the problem.
 */
export function CodeEditor({
  label,
  description,
  value,
  onChange,
  height = 360,
  readOnly = false,
  markers = noMarkers,
  language = 'json',
  schema,
  error,
}: CodeEditorProps) {
  const { t } = useTranslation();
  const id = useId();
  const focusRef = useRef<((line: number, column: number) => void) | null>(null);
  const onReady = useCallback((api: { focusAt: (line: number, column: number) => void }) => {
    focusRef.current = api.focusAt;
  }, []);

  const implProps: EditorImplProps = {
    value,
    onChange,
    ariaLabel: label,
    height,
    readOnly,
    markers,
    language,
    id: `${id}-editor`,
    onReady,
    ...(schema !== undefined ? { schema } : {}),
  };
  const plain = <PlainCodeEditor {...implProps} />;

  return (
    <Input.Wrapper
      label={label}
      labelProps={{ htmlFor: `${id}-editor` }}
      description={description}
      error={error}
    >
      <MonacoBoundary fallback={plain}>
        <Suspense fallback={<Skeleton h={height} data-testid="code-editor-loading" />}>
          <MonacoCodeEditor {...implProps} />
        </Suspense>
      </MonacoBoundary>
      {markers.length > 0 ? (
        <List
          size="sm"
          mt="xs"
          spacing={2}
          aria-label={t('codeEditor.problems')}
          role="list"
          c="red"
        >
          {markers.map((marker, index) => (
            <List.Item key={`${marker.line}:${marker.column}:${index}`}>
              <Text span size="sm" c="red">
                {marker.line > 0 ? (
                  <Anchor
                    component="button"
                    type="button"
                    size="sm"
                    c="red"
                    onClick={() => {
                      focusRef.current?.(marker.line, marker.column);
                    }}
                  >
                    {t('codeEditor.position', { line: marker.line, column: marker.column })}
                  </Anchor>
                ) : null}
                {marker.line > 0 ? ': ' : ''}
                {marker.message}
              </Text>
            </List.Item>
          ))}
        </List>
      ) : null}
    </Input.Wrapper>
  );
}
