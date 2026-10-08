import { useMemo, type ReactNode } from 'react';

import { CodeEditor } from '@/components/code/CodeEditor';
import type { CodeMarker } from '@/components/code/types';
import { lintPolicy, type PolicyKind } from '@/lib/policyLint';
import { buildPolicySchema, policySchemaUris } from '@/lib/policySchema';
import type { LocatedError } from '@/services';

export interface PolicyJsonEditorProps {
  label: string;
  description?: ReactNode;
  value: string;
  onChange: (value: string) => void;
  /** identity (IAM, inline) or bucket policy. */
  kind?: PolicyKind;
  /** Bucket of a bucket policy (resources must stay inside it). */
  bucket?: string;
  /** Located errors answered by the server for the current text. */
  serverErrors?: readonly LocatedError[];
  readOnly?: boolean;
  height?: number;
  /** Lint the text while typing (default true). */
  lint?: boolean;
}

/** Converts located policy errors to editor markers. */
function markersOf(errors: readonly LocatedError[]): CodeMarker[] {
  return errors.map((e) => ({
    line: e.line,
    column: e.column,
    message:
      e.element !== undefined && e.element !== '' && e.element !== 'JSON'
        ? `${e.element}: ${e.message}`
        : e.message,
  }));
}

/**
 * JSON editor of a policy document: Monaco with the AWS policy schema
 * (completion of actions, operators and condition keys), the client-side
 * lint while typing and the errors located by the server.
 */
export function PolicyJsonEditor({
  label,
  description,
  value,
  onChange,
  kind = 'identity',
  bucket,
  serverErrors,
  readOnly = false,
  height,
  lint = true,
}: PolicyJsonEditorProps) {
  const schema = useMemo(
    () => ({ uri: policySchemaUris[kind], schema: buildPolicySchema(kind) }),
    [kind],
  );
  const markers = useMemo(() => {
    const local =
      lint && !readOnly
        ? lintPolicy(value, { kind, ...(bucket !== undefined ? { bucket } : {}) })
        : [];
    const remote = serverErrors ?? [];
    // Server errors win: drop the local duplicates of the same position.
    const seen = new Set(remote.map((e) => `${e.line}:${e.column}:${e.message}`));
    return markersOf([
      ...remote,
      ...local.filter((e) => !seen.has(`${e.line}:${e.column}:${e.message}`)),
    ]);
  }, [value, kind, bucket, serverErrors, lint, readOnly]);

  return (
    <CodeEditor
      label={label}
      description={description}
      value={value}
      onChange={onChange}
      readOnly={readOnly}
      markers={markers}
      schema={schema}
      {...(height !== undefined ? { height } : {})}
    />
  );
}
