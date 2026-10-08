import { Textarea } from '@mantine/core';
import { useEffect, useRef } from 'react';

import type { EditorImplProps } from './types';

/**
 * Textarea implementation of the code editor: used while Monaco loads in
 * tests (jsdom cannot run Monaco) and when Monaco fails to load.
 */
export function PlainCodeEditor({
  value,
  onChange,
  ariaLabel,
  height,
  readOnly,
  markers,
  id,
  onReady,
}: EditorImplProps) {
  const ref = useRef<HTMLTextAreaElement>(null);

  useEffect(() => {
    onReady?.({
      focusAt(line, column) {
        const area = ref.current;
        if (!area) return;
        const lines = area.value.split('\n');
        let offset = 0;
        for (let i = 0; i < line - 1 && i < lines.length; i += 1) {
          offset += (lines[i]?.length ?? 0) + 1;
        }
        offset += Math.max(0, column - 1);
        area.focus();
        area.setSelectionRange(offset, offset);
      },
    });
  }, [onReady]);

  return (
    <Textarea
      ref={ref}
      id={id}
      aria-label={ariaLabel}
      error={markers.length > 0}
      value={value}
      readOnly={readOnly}
      onChange={(event) => {
        onChange(event.currentTarget.value);
      }}
      spellCheck={false}
      autoComplete="off"
      styles={{
        input: {
          fontFamily: 'var(--mantine-font-family-monospace)',
          fontSize: 13,
          height,
          minHeight: height,
        },
      }}
    />
  );
}

export default PlainCodeEditor;
