/**
 * Composition root of the console services: picks the implementations
 * (HTTP against the Admin API or in-memory mocks for the demo mode) and
 * re-exports the public types. Components import from here or from
 * ./interfaces, never from ./http or ./mock.
 */
import { createHttpServices } from './http';
import type { Services } from './interfaces';
import { createMockServices } from './mock';

export interface ServicesConfig {
  /** Use the in-memory mocks instead of the Admin API. */
  demo: boolean;
  /** Base path of the Admin API, e.g. "/api/v1". */
  apiBase: string;
}

/** Builds the service bundle for the given configuration. */
export function createServices(config: ServicesConfig): Services {
  if (config.demo) {
    return createMockServices({ latencyMs: 150 });
  }
  return createHttpServices({ baseUrl: config.apiBase });
}

export { ApiError, isApiError, type ProblemDetails } from './errors';
export { demoCredentials } from './mock';
export type * from './interfaces';
export { ServicesProvider, useServices } from './ServicesProvider';
export type * from './types';
