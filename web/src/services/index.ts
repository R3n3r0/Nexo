/**
 * Composition root of the console services: picks the implementations
 * (HTTP against the Admin API or in-memory mocks for the demo mode) and
 * re-exports the public types. Components import from here or from
 * ./interfaces, never from ./http or ./mock.
 */
import { createHttpServices } from './http';
import type { Services } from './interfaces';

export interface ServicesConfig {
  /** Use the in-memory mocks instead of the Admin API. */
  demo: boolean;
  /** Base path of the Admin API, e.g. "/api/v1". */
  apiBase: string;
}

/**
 * Builds the service bundle for the given configuration. The mocks of the
 * demo mode are loaded on demand: they never weigh on the initial bundle
 * of the console served by Nexo.
 */
export async function loadServices(config: ServicesConfig): Promise<Services> {
  if (config.demo) {
    const { createMockServices } = await import('./mock');
    return createMockServices({ latencyMs: 150 });
  }
  return createHttpServices({ baseUrl: config.apiBase });
}

export {
  ApiError,
  isApiError,
  type AccessRuleProblem,
  type LocatedError,
  type ProblemDetails,
  type RuleProblem,
} from './errors';
export { demoCredentials } from './demo';
export type * from './interfaces';
export { ServicesProvider, useServices } from './ServicesProvider';
export type * from './types';
