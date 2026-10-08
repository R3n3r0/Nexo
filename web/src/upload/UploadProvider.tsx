import { useQueryClient } from '@tanstack/react-query';
import {
  createContext,
  useContext,
  useEffect,
  useState,
  useSyncExternalStore,
  type ReactNode,
} from 'react';

import { storageKeys } from '@/lib/storageKeys';
import { useServices } from '@/services';

import { createUploadManager, type UploadManager, type UploadSnapshot } from './manager';

const UploadContext = createContext<UploadManager | null>(null);

export interface UploadProviderProps {
  children: ReactNode;
  /** Manager to use (tests); one is created from the services otherwise. */
  manager?: UploadManager;
}

/**
 * Owns the upload queue of the session. Mounted around the authenticated
 * pages so that uploads survive the navigation between them; it refreshes
 * the listings when a file is stored and warns before the page is left
 * while uploads are active.
 */
export function UploadProvider({ children, manager: provided }: UploadProviderProps) {
  const { uploads } = useServices();
  const queryClient = useQueryClient();
  const [manager] = useState(() => provided ?? createUploadManager({ uploads }));

  useEffect(
    () =>
      manager.onCompleted((item) => {
        void queryClient.invalidateQueries({ queryKey: storageKeys.objects(item.bucket) });
        void queryClient.invalidateQueries({ queryKey: storageKeys.buckets });
      }),
    [manager, queryClient],
  );

  // Leaving the authenticated area (sign-out) cancels the queue and aborts
  // the multipart uploads; the manager itself stays usable.
  useEffect(
    () => () => {
      void manager.dispose();
    },
    [manager],
  );

  useEffect(() => {
    const onBeforeUnload = (event: BeforeUnloadEvent) => {
      if (manager.getSnapshot().active > 0) {
        event.preventDefault();
        // Legacy browsers show the prompt only when returnValue is set.
        event.returnValue = '';
      }
    };
    window.addEventListener('beforeunload', onBeforeUnload);
    return () => {
      window.removeEventListener('beforeunload', onBeforeUnload);
    };
  }, [manager]);

  return <UploadContext.Provider value={manager}>{children}</UploadContext.Provider>;
}

/** The upload queue of the nearest UploadProvider. */
// eslint-disable-next-line react-refresh/only-export-components -- hooks belong with the provider
export function useUploadManager(): UploadManager {
  const manager = useContext(UploadContext);
  if (manager === null) {
    throw new Error('useUploadManager() must be used within an <UploadProvider>');
  }
  return manager;
}

/** Re-renders on every change of the queue. */
// eslint-disable-next-line react-refresh/only-export-components -- hooks belong with the provider
export function useUploadSnapshot(): UploadSnapshot {
  const manager = useUploadManager();
  return useSyncExternalStore(manager.subscribe, manager.getSnapshot, manager.getSnapshot);
}
