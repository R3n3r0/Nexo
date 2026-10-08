import { isRouteErrorResponse, useRouteError } from 'react-router';

import { NotFoundPage } from '@/pages/NotFoundPage';

import { ErrorFallback } from './ErrorBoundary';

/** errorElement of the data router: 404 responses and render errors. */
export function RouteErrorPage() {
  const error = useRouteError();
  if (isRouteErrorResponse(error)) {
    if (error.status === 404) {
      return <NotFoundPage />;
    }
    return <ErrorFallback error={new Error(`${error.status} ${error.statusText}`)} />;
  }
  return <ErrorFallback error={error instanceof Error ? error : new Error(String(error))} />;
}
