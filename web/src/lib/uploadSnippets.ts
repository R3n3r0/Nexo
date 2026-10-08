/**
 * Ready-to-paste snippets for presigned uploads (D-SHR-02): curl, HTML
 * form and JavaScript for a presigned PUT URL or a POST policy. Values are
 * quoted for their language so that keys and URLs with special characters
 * stay correct.
 */
import type { PostPolicy, PresignedUrl } from '@/services';

/** Single-quotes a value for POSIX shells. */
export function shellQuote(value: string): string {
  return `'${value.replace(/'/g, `'\\''`)}'`;
}

/** Escapes a value for an HTML attribute. */
export function htmlAttribute(value: string): string {
  return value
    .replace(/&/g, '&amp;')
    .replace(/"/g, '&quot;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;');
}

/** A JavaScript string literal. */
const jsString = (value: string) => JSON.stringify(value);

export interface Snippets {
  curl: string;
  javascript: string;
  html?: string;
}

/** Snippets uploading ./file with a presigned PUT URL. */
export function putSnippets(presigned: PresignedUrl): Snippets {
  const headers = Object.entries(presigned.headers);
  const curl = [
    'curl -X PUT',
    ...headers.map(([k, v]) => `  -H ${shellQuote(`${k}: ${v}`)}`),
    '  --upload-file ./file',
    `  ${shellQuote(presigned.url)}`,
  ].join(' \\\n');
  const headerLines =
    headers.length > 0
      ? `\n  headers: {\n${headers.map(([k, v]) => `    ${jsString(k)}: ${jsString(v)},`).join('\n')}\n  },`
      : '';
  const javascript = [
    '// file: a File or Blob, e.g. from <input type="file">',
    `const response = await fetch(${jsString(presigned.url)}, {`,
    `  method: 'PUT',${headerLines}`,
    '  body: file,',
    '});',
    'if (!response.ok) throw new Error(`Upload failed: ${response.status}`);',
  ].join('\n');
  return { curl, javascript };
}

/** Snippets uploading a file with a POST policy form. */
export function postSnippets(policy: PostPolicy): Snippets {
  const fields = Object.entries(policy.fields);
  const html = [
    `<form action="${htmlAttribute(policy.url)}" method="post" enctype="multipart/form-data">`,
    ...fields.map(
      ([k, v]) =>
        `  <input type="hidden" name="${htmlAttribute(k)}" value="${htmlAttribute(v)}" />`,
    ),
    '  <input type="file" name="file" />',
    '  <button type="submit">Upload</button>',
    '</form>',
  ].join('\n');
  // --form-string sends the values verbatim: with -F a value starting
  // with "@" or "<" (a key prefix "@inbox/") would read a local file.
  const curl = [
    'curl -X POST',
    ...fields.map(([k, v]) => `  --form-string ${shellQuote(`${k}=${v}`)}`),
    '  -F file=@./file',
    `  ${shellQuote(policy.url)}`,
  ].join(' \\\n');
  const javascript = [
    '// file: a File, e.g. from <input type="file">',
    'const form = new FormData();',
    ...fields.map(([k, v]) => `form.append(${jsString(k)}, ${jsString(v)});`),
    "form.append('file', file); // the file must be the last field",
    `const response = await fetch(${jsString(policy.url)}, { method: 'POST', body: form });`,
    'if (!response.ok) throw new Error(`Upload failed: ${response.status}`);',
  ].join('\n');
  return { curl, javascript, html };
}
