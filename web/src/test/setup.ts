import '@testing-library/jest-dom/vitest';

import { cleanup } from '@testing-library/react';
import { afterAll, afterEach, beforeAll, vi } from 'vitest';

import i18n, { initI18n } from '@/i18n';
import enSections from '@/i18n/locales/en.sections.json';

import { server } from './server';

// Tests assert on English copy (bundled: ready without waiting), the
// strings of the sections loaded on demand included: the tests render the
// pages without their route loaders.
void initI18n({ lng: 'en' });
i18n.addResourceBundle('en', 'translation', enSections, true, false);

// Monaco cannot run in jsdom: code editors use their plain textarea
// implementation, which has the same props and markers.
vi.mock('@/components/code/MonacoCodeEditor', async () => {
  const plain = await import('@/components/code/PlainCodeEditor');
  return { default: plain.PlainCodeEditor, MonacoCodeEditor: plain.PlainCodeEditor };
});

// Browser APIs used by Mantine that jsdom does not implement. Plain
// functions, not vi.fn(): restoreMocks would wipe a mock before each test.
const noop = () => {};
Object.defineProperty(window, 'matchMedia', {
  writable: true,
  value: (query: string): MediaQueryList => ({
    matches: false,
    media: query,
    onchange: null,
    addListener: noop,
    removeListener: noop,
    addEventListener: noop,
    removeEventListener: noop,
    dispatchEvent: () => false,
  }),
});

class ResizeObserverMock {
  observe() {}
  unobserve() {}
  disconnect() {}
}
window.ResizeObserver = ResizeObserverMock;
window.HTMLElement.prototype.scrollIntoView = noop;

beforeAll(() => {
  server.listen({ onUnhandledRequest: 'error' });
});

afterEach(() => {
  cleanup();
  server.resetHandlers();
  document.cookie = 'nexo_csrf=; expires=Thu, 01 Jan 1970 00:00:00 GMT';
});

afterAll(() => {
  server.close();
});
