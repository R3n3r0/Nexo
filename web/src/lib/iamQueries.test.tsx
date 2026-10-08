import { QueryClientProvider, useQuery } from '@tanstack/react-query';
import { act, renderHook, waitFor } from '@testing-library/react';
import type { ReactNode } from 'react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { createTestQueryClient } from '@/test/render';

import { readTextFile, saveJsonFile } from './saveFile';
import { iamKeys, useInvalidatingMutation } from './iamQueries';

describe('useInvalidatingMutation (TD-U-14)', () => {
  it('refreshes the listed queries after a success, before onSuccess', async () => {
    const client = createTestQueryClient();
    const wrapper = ({ children }: { children: ReactNode }) => (
      <QueryClientProvider client={client}>{children}</QueryClientProvider>
    );
    let users = ['alice'];
    const order: string[] = [];
    const { result } = renderHook(
      () => {
        const list = useQuery({
          queryKey: iamKeys.users,
          queryFn: () => {
            order.push('fetch');
            return Promise.resolve([...users]);
          },
          staleTime: Infinity,
        });
        const create = useInvalidatingMutation({
          mutationFn: (name: string) => {
            users = [...users, name];
            return Promise.resolve(name);
          },
          invalidate: [iamKeys.users],
          onSuccess: () => {
            order.push('onSuccess');
          },
        });
        return { list, create };
      },
      { wrapper },
    );
    await waitFor(() => {
      expect(result.current.list.data).toEqual(['alice']);
    });
    await act(async () => {
      await result.current.create.mutateAsync('bob');
    });
    await waitFor(() => {
      expect(result.current.list.data).toEqual(['alice', 'bob']);
    });
    expect(order).toEqual(['fetch', 'fetch', 'onSuccess']);
  });

  it('does not invalidate after a failure and exposes the error', async () => {
    const client = createTestQueryClient();
    const invalidate = vi.spyOn(client, 'invalidateQueries');
    const wrapper = ({ children }: { children: ReactNode }) => (
      <QueryClientProvider client={client}>{children}</QueryClientProvider>
    );
    const { result } = renderHook(
      () =>
        useInvalidatingMutation({
          mutationFn: () => Promise.reject(new Error('boom')),
          invalidate: [iamKeys.users],
        }),
      { wrapper },
    );
    await act(async () => {
      await result.current.mutateAsync().catch(() => undefined);
    });
    await waitFor(() => {
      expect(result.current.error?.message).toBe('boom');
    });
    expect(invalidate).not.toHaveBeenCalled();
  });
});

describe('saveJsonFile', () => {
  const descriptors = {
    create: Object.getOwnPropertyDescriptor(URL, 'createObjectURL'),
    revoke: Object.getOwnPropertyDescriptor(URL, 'revokeObjectURL'),
  };
  const restore = (name: 'createObjectURL' | 'revokeObjectURL', d?: PropertyDescriptor) => {
    if (d) Object.defineProperty(URL, name, d);
    else Reflect.deleteProperty(URL, name);
  };
  afterEach(() => {
    restore('createObjectURL', descriptors.create);
    restore('revokeObjectURL', descriptors.revoke);
  });

  it('downloads the pretty-printed document through a blob URL', async () => {
    const blobs: Blob[] = [];
    const createObjectURL = vi.fn((blob: Blob) => {
      blobs.push(blob);
      return 'blob:test';
    });
    const revokeObjectURL = vi.fn();
    Object.defineProperty(URL, 'createObjectURL', { value: createObjectURL, configurable: true });
    Object.defineProperty(URL, 'revokeObjectURL', { value: revokeObjectURL, configurable: true });
    const click = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {});
    saveJsonFile('creds.json', { accessKey: 'A' });
    expect(click).toHaveBeenCalledTimes(1);
    const blob = blobs[0];
    if (!blob) throw new Error('no blob');
    expect(await readTextFile(blob)).toBe('{\n  "accessKey": "A"\n}\n');
    expect(blob.type).toBe('application/json');
    // The blob URL is revoked once the browser had time to start the download.
    await waitFor(
      () => {
        expect(revokeObjectURL).toHaveBeenCalledWith('blob:test');
      },
      { timeout: 3_000 },
    );
  });
});
