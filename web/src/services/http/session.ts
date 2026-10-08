import type { SessionService } from '../interfaces';
import type { Session } from '../types';
import type { HttpClient } from './client';

/** SessionService over POST /auth/login, POST /auth/logout, GET /auth/session. */
export function createHttpSessionService(client: HttpClient): SessionService {
  return {
    async login(accessKey, secretKey) {
      const session = await client.request<Session>('/auth/login', {
        method: 'POST',
        body: { accessKey, secretKey },
        notifyUnauthorized: false,
      });
      client.setCsrfToken(session.csrfToken);
      return session;
    },
    async logout() {
      try {
        await client.request<void>('/auth/logout', { method: 'POST', notifyUnauthorized: false });
      } finally {
        client.setCsrfToken(undefined);
      }
    },
    async current() {
      const session = await client.request<Session>('/auth/session');
      client.setCsrfToken(session.csrfToken);
      return session;
    },
  };
}
