/** A problem to highlight in a code editor (1-based line and column). */
export interface CodeMarker {
  line: number;
  column: number;
  message: string;
  severity?: 'error' | 'warning';
}

/** A JSON schema bound to an editor (validation and completion, Monaco only). */
export interface CodeSchema {
  /** Stable identifier of the schema, e.g. "nexo://schemas/iam-policy.json". */
  uri: string;
  schema: unknown;
}

/** Props shared by the Monaco editor and its plain textarea fallback. */
export interface EditorImplProps {
  value: string;
  onChange: (value: string) => void;
  /** Accessible name of the editing surface. */
  ariaLabel: string;
  /** Height in pixels. */
  height: number;
  readOnly: boolean;
  markers: readonly CodeMarker[];
  language: 'json' | 'plaintext';
  schema?: CodeSchema;
  id?: string;
  /** Called with the editor's focus function once it is mounted. */
  onReady?: (api: { focusAt: (line: number, column: number) => void }) => void;
}
