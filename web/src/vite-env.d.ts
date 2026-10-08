/// <reference types="vite/client" />

interface ImportMetaEnv {
  /** "1" enables the demo mode with in-memory mock services. */
  readonly VITE_DEMO?: string;
  /** Base path of the Admin API; defaults to /api/v1. */
  readonly VITE_API_BASE?: string;
}

interface ImportMeta {
  readonly env: ImportMetaEnv;
}
