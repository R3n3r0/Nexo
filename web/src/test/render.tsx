import { MantineProvider } from '@mantine/core';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, type RenderOptions, type RenderResult } from '@testing-library/react';
import { useState, type ReactElement, type ReactNode } from 'react';
import { createMemoryRouter, RouterProvider, type RouteObject } from 'react-router';

import { SessionProvider } from '@/auth/SessionProvider';
import { ServicesProvider, type Services } from '@/services';
import { createMockServices, type MockServices } from '@/services/mock';
import { theme } from '@/theme';

export interface ProviderOptions {
  services?: Services;
  queryClient?: QueryClient;
}

export function createTestQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 }, mutations: { retry: false } },
  });
}

/**
 * Mantine + Query + Services + Session, without a router. Defaults are
 * created once per mounted tree, never per render.
 */
export function TestProviders({
  children,
  services,
  queryClient,
}: ProviderOptions & { children: ReactNode }) {
  const [client] = useState(() => queryClient ?? createTestQueryClient());
  const [svc] = useState(() => services ?? createMockServices());
  return (
    <MantineProvider theme={theme} env="test">
      <QueryClientProvider client={client}>
        <ServicesProvider services={svc}>
          <SessionProvider>{children}</SessionProvider>
        </ServicesProvider>
      </QueryClientProvider>
    </MantineProvider>
  );
}

export interface RenderWithProvidersOptions extends ProviderOptions {
  renderOptions?: Omit<RenderOptions, 'wrapper'>;
}

/** Renders an element inside the providers (no router). */
export function renderWithProviders(
  ui: ReactElement,
  { services, queryClient, renderOptions }: RenderWithProvidersOptions = {},
): RenderResult & { services: Services } {
  const svc = services ?? createMockServices();
  const result = render(ui, {
    wrapper: ({ children }) => (
      <TestProviders services={svc} {...(queryClient ? { queryClient } : {})}>
        {children}
      </TestProviders>
    ),
    ...renderOptions,
  });
  return { ...result, services: svc };
}

export interface RenderRoutesOptions extends ProviderOptions {
  initialEntries?: string[];
}

/** Renders a route table inside the providers with an in-memory router. */
export function renderRoutes(
  routes: RouteObject[],
  { initialEntries = ['/'], services, queryClient }: RenderRoutesOptions = {},
) {
  const svc = services ?? createMockServices();
  const router = createMemoryRouter(routes, { initialEntries });
  const result = render(
    <TestProviders services={svc} {...(queryClient ? { queryClient } : {})}>
      <RouterProvider router={router} />
    </TestProviders>,
  );
  return { ...result, router, services: svc };
}

export type { MockServices };
