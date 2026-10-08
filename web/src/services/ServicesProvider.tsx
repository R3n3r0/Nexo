import { createContext, useContext, type ReactNode } from 'react';

import type { Services } from './interfaces';

const ServicesContext = createContext<Services | null>(null);

export interface ServicesProviderProps {
  services: Services;
  children: ReactNode;
}

/**
 * Makes a bundle of services available to the React tree. The application
 * mounts it once with the HTTP implementations (or the mocks in demo mode);
 * tests mount it with the mocks.
 */
export function ServicesProvider({ services, children }: ServicesProviderProps) {
  return <ServicesContext.Provider value={services}>{children}</ServicesContext.Provider>;
}

/**
 * Returns the services of the nearest ServicesProvider. Components never
 * import an implementation: this hook is their only access to the backend.
 */
// eslint-disable-next-line react-refresh/only-export-components -- hook and provider belong together
export function useServices(): Services {
  const services = useContext(ServicesContext);
  if (services === null) {
    throw new Error('useServices() must be used within a <ServicesProvider>');
  }
  return services;
}
