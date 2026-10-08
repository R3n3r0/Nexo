import '@mantine/core/styles.css';
import '@mantine/notifications/styles.css';
import 'dayjs/locale/it';

import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';

import { App } from '@/App';
import { readAppConfig } from '@/config';
import { detectLanguage, initI18n, languageStorageKey } from '@/i18n';
import { createAppRouter } from '@/router';
import { demoCredentials, loadServices } from '@/services';

function readStoredLanguage(): string | null {
  try {
    return localStorage.getItem(languageStorageKey);
  } catch {
    return null;
  }
}

const config = readAppConfig();
await initI18n({ lng: detectLanguage(readStoredLanguage(), navigator.languages) });

const services = await loadServices(config);
const router = createAppRouter(config.demo ? { login: { demoCredentials } } : {});

const rootElement = document.getElementById('root');
if (!rootElement) {
  throw new Error('Missing #root element in index.html');
}

createRoot(rootElement).render(
  <StrictMode>
    <App services={services} router={router} />
  </StrictMode>,
);
