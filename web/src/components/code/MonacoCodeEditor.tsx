/**
 * Monaco implementation of the code editor. This module is only reached
 * through a dynamic import (CodeEditor), so Monaco and its workers stay out
 * of the initial bundle (docs/06 §6.5, bundle budget). Monaco is bundled
 * from the monaco-editor package: nothing is fetched from a CDN, which the
 * console Content-Security-Policy would block anyway.
 */
import 'monaco-editor/esm/vs/editor/edcore.main';
import 'monaco-editor/esm/vs/language/json/monaco.contribution';
import 'monaco-editor/esm/vs/basic-languages/xml/xml.contribution';

import { Box, useComputedColorScheme } from '@mantine/core';
import * as monaco from 'monaco-editor/esm/vs/editor/editor.api';
import EditorWorker from 'monaco-editor/esm/vs/editor/editor.worker?worker';
import JsonWorker from 'monaco-editor/esm/vs/language/json/json.worker?worker';
import { useEffect, useId, useRef } from 'react';

import type { CodeSchema, EditorImplProps } from './types';

self.MonacoEnvironment = {
  getWorker(_workerId: string, label: string) {
    return label === 'json' ? new JsonWorker() : new EditorWorker();
  },
};

/** Schemas bound to the models that use them (json diagnostics are global). */
const boundSchemas = new Map<string, { schema: unknown; models: Set<string> }>();

function applySchemas() {
  monaco.languages.json.jsonDefaults.setDiagnosticsOptions({
    validate: true,
    allowComments: false,
    enableSchemaRequest: false,
    schemaValidation: 'error',
    schemas: [...boundSchemas.entries()]
      .filter(([, entry]) => entry.models.size > 0)
      .map(([uri, entry]) => ({ uri, fileMatch: [...entry.models], schema: entry.schema })),
  });
}

function bindSchema(schema: CodeSchema, modelUri: string): () => void {
  const entry = boundSchemas.get(schema.uri) ?? { schema: schema.schema, models: new Set() };
  entry.schema = schema.schema;
  entry.models.add(modelUri);
  boundSchemas.set(schema.uri, entry);
  applySchemas();
  return () => {
    entry.models.delete(modelUri);
    applySchemas();
  };
}

const markerOwner = 'nexo';

export function MonacoCodeEditor({
  value,
  onChange,
  ariaLabel,
  height,
  readOnly,
  markers,
  language,
  schema,
  id,
  onReady,
}: EditorImplProps) {
  const containerRef = useRef<HTMLDivElement>(null);
  const editorRef = useRef<monaco.editor.IStandaloneCodeEditor | null>(null);
  const onChangeRef = useRef(onChange);
  onChangeRef.current = onChange;
  const reactId = useId();
  const scheme = useComputedColorScheme('light');

  // Create the editor once; props are synchronised by the effects below.
  useEffect(() => {
    const container = containerRef.current;
    if (!container) return undefined;
    const uri = monaco.Uri.parse(`inmemory://nexo/${encodeURIComponent(reactId)}.${language}`);
    const model = monaco.editor.createModel(value, language, uri);
    const editor = monaco.editor.create(container, {
      model,
      ariaLabel,
      automaticLayout: true,
      minimap: { enabled: false },
      scrollBeyondLastLine: false,
      tabSize: 2,
      fontSize: 13,
      readOnly,
      fixedOverflowWidgets: true,
      quickSuggestions: { strings: true, other: true, comments: false },
      // Paste the text of the paste event, synchronously. With "paste as"
      // on, Monaco takes over every paste and first calls
      // navigator.clipboard.read() to look for copied resources: in Chrome
      // that asks the user for the clipboard-read permission and holds the
      // paste until it is answered (forever where no one can answer, e.g.
      // a headless browser), so the pasted text never reaches the editor.
      // The console has no paste providers that would use it.
      pasteAs: { enabled: false },
    });
    editorRef.current = editor;
    const subscription = model.onDidChangeContent(() => {
      onChangeRef.current(model.getValue());
    });
    onReady?.({
      focusAt(line, column) {
        editor.focus();
        editor.setPosition({ lineNumber: Math.max(1, line), column: Math.max(1, column) });
        editor.revealLineInCenter(Math.max(1, line));
      },
    });
    return () => {
      subscription.dispose();
      editor.dispose();
      model.dispose();
      editorRef.current = null;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps -- created once, synced below
  }, []);

  // Controlled value: only push external changes, never echo typing.
  useEffect(() => {
    const model = editorRef.current?.getModel();
    if (model && model.getValue() !== value) {
      model.pushEditOperations([], [{ range: model.getFullModelRange(), text: value }], () => null);
    }
  }, [value]);

  useEffect(() => {
    editorRef.current?.updateOptions({ readOnly, ariaLabel });
  }, [readOnly, ariaLabel]);

  useEffect(() => {
    monaco.editor.setTheme(scheme === 'dark' ? 'vs-dark' : 'vs');
  }, [scheme]);

  useEffect(() => {
    const model = editorRef.current?.getModel();
    if (!schema || !model) return undefined;
    return bindSchema(schema, model.uri.toString());
  }, [schema]);

  useEffect(() => {
    const model = editorRef.current?.getModel();
    if (!model) return;
    monaco.editor.setModelMarkers(
      model,
      markerOwner,
      markers.map((marker) => {
        const line = Math.min(Math.max(1, marker.line), model.getLineCount());
        const column = Math.max(1, marker.column);
        return {
          severity:
            marker.severity === 'warning'
              ? monaco.MarkerSeverity.Warning
              : monaco.MarkerSeverity.Error,
          message: marker.message,
          startLineNumber: line,
          startColumn: marker.column > 0 ? column : 1,
          endLineNumber: line,
          endColumn: marker.column > 0 ? column + 1 : model.getLineMaxColumn(line),
        };
      }),
    );
  }, [markers, value]);

  return (
    <Box
      ref={containerRef}
      id={id}
      h={height}
      style={{
        border: '1px solid var(--mantine-color-default-border)',
        borderRadius: 'var(--mantine-radius-sm)',
        overflow: 'hidden',
      }}
    />
  );
}

export default MonacoCodeEditor;
