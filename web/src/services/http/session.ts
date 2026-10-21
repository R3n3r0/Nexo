import type { SessionService } from '../interfaces';
import type { AuthProviders, Session } from '../types';
import type { HttpClient } from './client';

/**
 * SessionService over POST /auth/login, POST /auth/logout, GET /auth/session
 * and the identity providers (GET /auth/providers, /auth/openid/start).
 */
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
    async loginLDAP(username, password) {
      const session = await client.request<Session>('/auth/login', {
        method: 'POST',
        body: { provider: 'ldap', username, password },
        notifyUnauthorized: false,
      });
      client.setCsrfToken(session.csrfToken);
      return session;
    },
    providers() {
      return client.request<AuthProviders>('/auth/providers', { notifyUnauthorized: false });
    },
    openIDLoginURL() {
      return client.url('/auth/openid/start');
    },
    async current() {
      const session = await client.request<Session>('/auth/session');
      client.setCsrfToken(session.csrfToken);
      return session;
    },
  };
}
