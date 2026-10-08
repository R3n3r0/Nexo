/** Clicks a temporary anchor: the browser downloads with the session cookie. */
function clickAnchor(href: string, filename?: string): void {
  const anchor = document.createElement('a');
  anchor.href = href;
  if (filename !== undefined) {
    anchor.download = filename;
  }
  anchor.rel = 'noopener';
  anchor.style.display = 'none';
  document.body.appendChild(anchor);
  anchor.click();
  anchor.remove();
}

/**
 * Starts the download of a URL served as an attachment by the Admin API
 * (GET requests need no CSRF token): the browser streams it to disk.
 */
export function downloadUrl(url: string, filename?: string): void {
  clickAnchor(url, filename);
}

/** Saves a blob produced by a request (zip archives) as a file. */
export function saveBlob(blob: Blob, filename: string): void {
  const url = URL.createObjectURL(blob);
  try {
    clickAnchor(url, filename);
  } finally {
    // Revoke later: some browsers start the download asynchronously.
    setTimeout(() => {
      URL.revokeObjectURL(url);
    }, 1_000);
  }
}
