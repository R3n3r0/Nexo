import { expect, type Locator, type Page } from '@playwright/test';

/**
 * Replaces the content of the Monaco editor labelled `label` (inside
 * `scope`) as a user pasting a document does: typing would trigger the
 * auto-indentation and the bracket auto-closing.
 */
export async function setEditorText(page: Page, scope: Locator, label: string, text: string) {
  await expect(scope.locator('.monaco-editor').first()).toBeVisible();
  const editor = scope.getByRole('textbox', { name: label });
  await editor.focus();
  await page.keyboard.press('ControlOrMeta+A');
  await editor.evaluate((element, value) => {
    const data = new DataTransfer();
    data.setData('text/plain', value);
    element.dispatchEvent(
      new ClipboardEvent('paste', { clipboardData: data, bubbles: true, cancelable: true }),
    );
  }, text);
}

/** The text the Monaco editor of `scope` shows (its visible lines). */
export function editorLines(scope: Locator): Locator {
  return scope.locator('.monaco-editor .view-lines').first();
}
