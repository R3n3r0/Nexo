import type { ServicesConfig } from './services';

/** Build-time configuration of the console, derived from Vite env variables. */
export type AppConfig = ServicesConfig;

export function readAppConfig(env: ImportMetaEnv = import.meta.env): AppConfig {
  return {
    demo: env.VITE_DEMO === '1' || env.VITE_DEMO === 'true',
    apiBase:
      env.VITE_API_BASE !== undefined && env.VITE_API_BASE !== '' ? env.VITE_API_BASE : '/api/v1',
  };
}
