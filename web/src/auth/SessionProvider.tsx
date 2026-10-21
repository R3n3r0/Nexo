import { useQueryClient } from '@tanstack/react-query';
import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from 'react';

import { useServices, type Session } from '@/services';

export type SessionStatus = 'loading' | 'authenticated' | 'anonymous';

export interface SessionState {
  status: SessionStatus;
  session: Session | null;
  /**
   * True when the session was lost while in use (any call answered 401):
   * the login page then explains that the session expired (TD-U-02).
   */
  expired: boolean;
}

export interface SessionContextValue extends SessionState {
  login: (accessKey: string, secretKey: string) => Promise<Session>;
  /** Opens a session for a user of the LDAP directory. */
  loginLDAP: (username: string, password: string) => Promise<Session>;
  logout: () => Promise<void>;
}

const SessionContext = createContext<SessionContextValue | null>(null);

/**
 * Holds the console session. On mount it asks the SessionService for the
 * current session; afterwards it reacts to login/logout and to the global
 * unauthorized event raised by any service call (expired session).
 */
export function SessionProvider({ children }: { children: ReactNode }) {
  const services = useServices();
  const queryClient = useQueryClient();
  const [state, setState] = useState<SessionState>({
    status: 'loading',
    session: null,
    expired: false,
  });
  // Mirrors state.status for the event listener, which must not capture a stale status.
  const statusRef = useRef<SessionStatus>('loading');
  statusRef.current = state.status;

  useEffect(() => {
    let cancelled = false;
    const dropSession = () => {
      if (cancelled) {
        return;
      }
      // Data fetched with a session must not survive its loss; nothing to
      // drop when we were never authenticated.
      const hadSession = statusRef.current === 'authenticated';
      statusRef.current = 'anonymous';
      setState({ status: 'anonymous', session: null, expired: hadSession });
      if (hadSession) {
        queryClient.clear();
      }
    };
    const unsubscribe = services.authEvents.onUnauthorized(dropSession);
    services.session.current().then(
      (session) => {
        if (!cancelled) {
          setState({ status: 'authenticated', session, expired: false });
        }
      },
      () => {
        dropSession();
      },
    );
    return () => {
      cancelled = true;
      unsubscribe();
    };
  }, [services, queryClient]);

  const login = useCallback(
    async (accessKey: string, secretKey: string) => {
      const session = await services.session.login(accessKey, secretKey);
      setState({ status: 'authenticated', session, expired: false });
      return session;
    },
    [services],
  );

  const loginLDAP = useCallback(
    async (username: string, password: string) => {
      const session = await services.session.loginLDAP(username, password);
      setState({ status: 'authenticated', session, expired: false });
      return session;
    },
    [services],
  );

  const logout = useCallback(async () => {
    try {
      await services.session.logout();
    } finally {
      setState({ status: 'anonymous', session: null, expired: false });
      queryClient.clear();
    }
  }, [services, queryClient]);

  const value = useMemo<SessionContextValue>(
    () => ({ ...state, login, loginLDAP, logout }),
    [state, login, loginLDAP, logout],
  );

  return <SessionContext.Provider value={value}>{children}</SessionContext.Provider>;
}

// eslint-disable-next-line react-refresh/only-export-components -- hook and provider belong together
export function useSession(): SessionContextValue {
  const value = useContext(SessionContext);
  if (value === null) {
    throw new Error('useSession() must be used within a <SessionProvider>');
  }
  return value;
}
