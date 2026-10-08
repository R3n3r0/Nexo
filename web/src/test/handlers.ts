import { http, HttpResponse, type HttpHandler } from 'msw';

import type { ProblemDetails } from '@/services';
import { sampleServerInfo } from '@/services/mock';
import type { Session } from '@/services/types';

/** Matches the Admin API on any origin (the client resolves against location). */
export const api = (path: string) => `*/api/v1${path}`;

/** Builds a problem+json response like httpx.WriteProblem. */
export function problemResponse(
  status: number,
  code: string,
  detail?: string,
  extra: Partial<ProblemDetails> = {},
) {
  const body: ProblemDetails = {
    title: statusText(status),
    status,
    code,
    ...(detail !== undefined ? { detail } : {}),
    requestId: 'req-test-1',
    ...extra,
  };
  return HttpResponse.json(body, {
    status,
    headers: { 'Content-Type': 'application/problem+json' },
  });
}

export const testCredentials = { accessKey: 'admin', secretKey: 'supersecret1' } as const;

export function sampleSession(accessKey = testCredentials.accessKey): Session {
  return {
    id: 'sess-1',
    accessKey,
    identity: { accessKey, isRoot: true, groups: [], policyNames: ['consoleAdmin'] },
    issuedAt: '2026-10-07T10:00:00Z',
    expiresAt: '2026-10-07T22:00:00Z',
    csrfToken: 'csrf-from-login',
  };
}

/** Baseline: no session, valid login with the test credentials, server info. */
export const defaultHandlers: HttpHandler[] = [
  http.get(api('/auth/session'), () =>
    problemResponse(401, 'unauthenticated', 'Authentication required.'),
  ),
  http.post(api('/auth/login'), async ({ request }) => {
    const body = (await request.json()) as { accessKey?: string; secretKey?: string };
    if (
      body.accessKey === testCredentials.accessKey &&
      body.secretKey === testCredentials.secretKey
    ) {
      return HttpResponse.json(sampleSession());
    }
    return problemResponse(401, 'invalid_credentials', 'invalid access key or secret key');
  }),
  http.post(api('/auth/logout'), () => new HttpResponse(null, { status: 204 })),
  http.get(api('/info'), () => HttpResponse.json(sampleServerInfo())),
  // The layout asks for the effective permissions of the signed-in user.
  http.get(api('/me/permissions'), () =>
    HttpResponse.json({
      user: testCredentials.accessKey,
      isRoot: true,
      allowed: [],
      denied: [],
    }),
  ),
];

function statusText(status: number): string {
  const titles: Record<number, string> = {
    400: 'Bad Request',
    401: 'Unauthorized',
    403: 'Forbidden',
    404: 'Not Found',
    409: 'Conflict',
    500: 'Internal Server Error',
  };
  return titles[status] ?? `HTTP ${status}`;
}
