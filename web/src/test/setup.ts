import '@testing-library/jest-dom/vitest';

import { cleanup } from '@testing-library/react';
import { afterAll, afterEach, beforeAll } from 'vitest';

import { initI18n } from '@/i18n';

import { server } from './server';

// Tests assert on English copy.
initI18n({ lng: 'en' });

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
