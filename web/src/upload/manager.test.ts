import { describe, expect, it, vi } from 'vitest';

import { ApiError, type UploadPartInput } from '@/services';
import { createMockServices } from '@/services/mock';

import { createUploadManager, type UploadManager } from './manager';
import { FileTooLargeError, partRanges, planParts } from './plan';

const KiB = 1024;
const MiB = 1024 * KiB;

/** Mock services announcing 1 KiB parts, so that tests use small blobs. */
function smallPartServices() {
  return createMockServices({
    uploadLimits: { partSize: KiB, minPartSize: KiB, maxPartSize: 4 * KiB, maxParts: 10_000 },
  });
}

function blob(size: number, type = 'application/octet-stream'): Blob {
  return new Blob([new Uint8Array(size)], { type });
}

/** Resolves once the predicate holds after the queue settles. */
async function until(manager: UploadManager, predicate: () => boolean, timeoutMs = 2_000) {
  const start = Date.now();
  while (!predicate()) {
    if (Date.now() - start > timeoutMs) {
      throw new Error(`timeout; snapshot: ${JSON.stringify(manager.getSnapshot())}`);
    }
    await new Promise((resolve) => setTimeout(resolve, 1));
  }
}

interface Deferred<T> {
  promise: Promise<T>;
  resolve: (value: T) => void;
  reject: (error: unknown) => void;
}

function deferred<T>(): Deferred<T> {
  let resolve!: (value: T) => void;
  let reject!: (error: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

// covers: D-OBJ-02
describe('planParts (TD-U-06: part sizes)', () => {
  const limits = {
    partSize: 16 * MiB,
    minPartSize: 5 * MiB,
    maxPartSize: 64 * MiB,
    maxParts: 10_000,
  };

  it.each([
    { size: 1, partSize: 16 * MiB, count: 1 },
    { size: 16 * MiB, partSize: 16 * MiB, count: 1 },
    { size: 16 * MiB + 1, partSize: 16 * MiB, count: 2 },
    { size: 100 * MiB, partSize: 16 * MiB, count: 7 },
    { size: 160_000 * MiB, partSize: 16 * MiB, count: 10_000 },
    // Past 10,000 recommended parts the size grows in whole MiB.
    { size: 200_000 * MiB, partSize: 20 * MiB, count: 10_000 },
    { size: 640_000 * MiB, partSize: 64 * MiB, count: 10_000 },
  ])('splits $size bytes in $count parts of $partSize', ({ size, partSize, count }) => {
    const plan = planParts(size, limits);
    expect(plan).toEqual({ partSize, count });
    expect(plan.count).toBeLessThanOrEqual(limits.maxParts);
    expect(plan.partSize).toBeGreaterThanOrEqual(limits.minPartSize);
  });

  it('refuses files beyond maxPartSize × maxParts', () => {
    expect(() => planParts(640_000 * MiB + 1, limits)).toThrow(FileTooLargeError);
  });

  it('never goes below the minimum part size', () => {
    expect(planParts(10 * MiB, { ...limits, partSize: MiB }).partSize).toBe(5 * MiB);
  });

  it('covers the file with contiguous ranges', () => {
    const ranges = partRanges(10, { partSize: 4, count: 3 });
    expect(ranges).toEqual([
      { partNumber: 1, start: 0, end: 4 },
      { partNumber: 2, start: 4, end: 8 },
      { partNumber: 3, start: 8, end: 10 },
    ]);
  });
});

// covers: D-OBJ-02
describe('createUploadManager (TD-U-06)', () => {
  it('uploads a small file in one request', async () => {
    const services = smallPartServices();
    const put = vi.spyOn(services.uploads, 'putObject');
    const manager = createUploadManager({
      uploads: services.uploads,
      singleUploadThreshold: 4 * KiB,
    });
    const completed = vi.fn();
    manager.onCompleted(completed);

    manager.enqueue('media', [{ key: 'docs/a b#1.txt', file: blob(100, 'text/plain') }]);
    await until(manager, () => manager.getSnapshot().items[0]?.status === 'completed');

    expect(put).toHaveBeenCalledTimes(1);
    expect(put.mock.calls[0]?.[0]).toMatchObject({
      bucket: 'media',
      key: 'docs/a b#1.txt',
      contentType: 'text/plain',
    });
    expect(completed).toHaveBeenCalledWith(
      expect.objectContaining({ key: 'docs/a b#1.txt', status: 'completed' }),
    );
    const stat = await services.objects.stat('media', 'docs/a b#1.txt');
    expect(stat.size).toBe(100);
  });

  it('splits a large file into parts and completes them in order', async () => {
    const services = smallPartServices();
    const complete = vi.spyOn(services.uploads, 'complete');
    const manager = createUploadManager({ uploads: services.uploads, singleUploadThreshold: KiB });

    manager.enqueue('media', [{ key: 'big.bin', file: blob(5 * KiB + 10) }]);
    await until(manager, () => manager.getSnapshot().items[0]?.status === 'completed');

    const item = manager.getSnapshot().items[0];
    expect(item).toMatchObject({ multipart: true, parts: 6, partsDone: 6, loaded: 5 * KiB + 10 });
    expect(complete).toHaveBeenCalledTimes(1);
    const parts = complete.mock.calls[0]?.[3] ?? [];
    expect(parts.map((p) => p.partNumber)).toEqual([1, 2, 3, 4, 5, 6]);
    expect((await services.objects.stat('media', 'big.bin')).size).toBe(5 * KiB + 10);
  });

  it('keeps at most `concurrency` requests in flight', async () => {
    const services = smallPartServices();
    let inFlight = 0;
    let peak = 0;
    const gates: Deferred<void>[] = [];
    const original = services.uploads.uploadPart.bind(services.uploads);
    vi.spyOn(services.uploads, 'uploadPart').mockImplementation(
      async (input: UploadPartInput, options) => {
        inFlight += 1;
        peak = Math.max(peak, inFlight);
        const gate = deferred<void>();
        gates.push(gate);
        await gate.promise;
        inFlight -= 1;
        return original(input, options);
      },
    );
    const manager = createUploadManager({
      uploads: services.uploads,
      singleUploadThreshold: KiB,
      concurrency: 4,
    });

    manager.enqueue('media', [
      { key: 'a.bin', file: blob(6 * KiB) },
      { key: 'b.bin', file: blob(6 * KiB) },
    ]);
    // Release the parts one by one as they arrive.
    let released = 0;
    while (released < 12) {
      await until(manager, () => gates.length > released);
      expect(inFlight).toBeLessThanOrEqual(4);
      gates[released]?.resolve();
      released += 1;
    }
    await until(manager, () => manager.getSnapshot().items.every((i) => i.status === 'completed'));
    expect(peak).toBe(4);
  });

  it('retries a failed part and reports progress in aggregate', async () => {
    const services = smallPartServices();
    const original = services.uploads.uploadPart.bind(services.uploads);
    let failures = 0;
    const part = vi.spyOn(services.uploads, 'uploadPart').mockImplementation((input, options) => {
      if (input.partNumber === 2 && failures === 0) {
        failures += 1;
        return Promise.reject(
          new ApiError({ title: 'Service Unavailable', status: 503, code: 'write_quorum' }),
        );
      }
      return original(input, options);
    });
    const manager = createUploadManager({
      uploads: services.uploads,
      singleUploadThreshold: KiB,
      retryDelay: () => 0,
    });

    manager.enqueue('media', [{ key: 'retry.bin', file: blob(3 * KiB) }]);
    await until(manager, () => manager.getSnapshot().items[0]?.status === 'completed');

    expect(part).toHaveBeenCalledTimes(4);
    expect(part.mock.calls.filter((c) => c[0].partNumber === 2)).toHaveLength(2);
    expect(manager.getSnapshot()).toMatchObject({
      totalBytes: 3 * KiB,
      loadedBytes: 3 * KiB,
      active: 0,
    });
  });

  it('fails after the attempts run out, then retry() sends only the missing parts', async () => {
    const services = smallPartServices();
    const original = services.uploads.uploadPart.bind(services.uploads);
    let broken = true;
    const part = vi.spyOn(services.uploads, 'uploadPart').mockImplementation((input, options) => {
      if (input.partNumber === 3 && broken) {
        return Promise.reject(ApiError.network(new Error('connection reset')));
      }
      return original(input, options);
    });
    const manager = createUploadManager({
      uploads: services.uploads,
      singleUploadThreshold: KiB,
      maxAttempts: 2,
      retryDelay: () => 0,
    });

    const [id = ''] = manager.enqueue('media', [{ key: 'flaky.bin', file: blob(3 * KiB) }]);
    await until(manager, () => manager.getSnapshot().items[0]?.status === 'failed');
    expect(manager.getSnapshot().items[0]).toMatchObject({
      partsDone: 2,
      errorCode: 'network_error',
    });
    expect(manager.getSnapshot().active).toBe(0);

    broken = false;
    part.mockClear();
    manager.retry(id);
    await until(manager, () => manager.getSnapshot().items[0]?.status === 'completed');
    expect(part.mock.calls.map((c) => c[0].partNumber)).toEqual([3]);
  });

  it('does not retry client errors', async () => {
    const services = smallPartServices();
    const part = vi
      .spyOn(services.uploads, 'uploadPart')
      .mockRejectedValue(new ApiError({ title: 'Forbidden', status: 403, code: 'access_denied' }));
    const manager = createUploadManager({
      uploads: services.uploads,
      singleUploadThreshold: KiB,
      retryDelay: () => 0,
    });

    manager.enqueue('media', [{ key: 'denied.bin', file: blob(2 * KiB) }]);
    await until(manager, () => manager.getSnapshot().items[0]?.status === 'failed');
    expect(part.mock.calls.filter((c) => c[0].partNumber === 1)).toHaveLength(1);
    expect(manager.getSnapshot().items[0]?.errorCode).toBe('access_denied');
  });

  it('pauses (aborting the requests in flight) and resumes', async () => {
    const services = smallPartServices();
    const original = services.uploads.uploadPart.bind(services.uploads);
    const gates: Deferred<void>[] = [];
    const signals: AbortSignal[] = [];
    vi.spyOn(services.uploads, 'uploadPart').mockImplementation(async (input, options = {}) => {
      if (options.signal) signals.push(options.signal);
      const gate = deferred<void>();
      gates.push(gate);
      options.signal?.addEventListener('abort', () => {
        gate.reject(ApiError.aborted());
      });
      await gate.promise;
      return original(input, options);
    });
    const complete = vi.spyOn(services.uploads, 'complete');
    const manager = createUploadManager({
      uploads: services.uploads,
      singleUploadThreshold: KiB,
      concurrency: 2,
    });

    const [id = ''] = manager.enqueue('media', [{ key: 'pause.bin', file: blob(4 * KiB) }]);
    await until(manager, () => gates.length === 2);
    gates[0]?.resolve();
    await until(
      manager,
      () => manager.getSnapshot().items[0]?.partsDone === 1 && gates.length === 3,
    );

    manager.pause(id);
    expect(manager.getSnapshot().items[0]?.status).toBe('paused');
    expect(signals.filter((s) => s.aborted)).toHaveLength(2);
    await new Promise((resolve) => setTimeout(resolve, 5));
    expect(gates).toHaveLength(3);

    manager.resume(id);
    await until(manager, () => gates.length === 5);
    gates[3]?.resolve();
    gates[4]?.resolve();
    await until(manager, () => gates.length === 6);
    gates[5]?.resolve();
    await until(manager, () => manager.getSnapshot().items[0]?.status === 'completed');
    expect(complete.mock.calls[0]?.[3].map((p) => p.partNumber)).toEqual([1, 2, 3, 4]);
  });

  it('cancel aborts the multipart upload on the server', async () => {
    const services = smallPartServices();
    const gate = deferred<void>();
    const original = services.uploads.uploadPart.bind(services.uploads);
    const part = vi
      .spyOn(services.uploads, 'uploadPart')
      .mockImplementation(async (input, options = {}) => {
        options.signal?.addEventListener('abort', () => {
          gate.reject(ApiError.aborted());
        });
        await gate.promise;
        return original(input, options);
      });
    const abort = vi.spyOn(services.uploads, 'abort');
    const complete = vi.spyOn(services.uploads, 'complete');
    const manager = createUploadManager({ uploads: services.uploads, singleUploadThreshold: KiB });

    const [id = ''] = manager.enqueue('media', [{ key: 'cancel.bin', file: blob(3 * KiB) }]);
    await until(manager, () => part.mock.calls.length > 0);
    expect((await services.uploads.list('media')).items).toHaveLength(1);
    await manager.cancel(id);

    expect(manager.getSnapshot().items[0]?.status).toBe('canceled');
    expect(abort).toHaveBeenCalledTimes(1);
    expect(abort.mock.calls[0]?.slice(0, 2)).toEqual(['media', 'cancel.bin']);
    expect(complete).not.toHaveBeenCalled();
    expect((await services.uploads.list('media')).items).toEqual([]);
  });

  // Regression (D-OBJ-02): canceling while CompleteMultipartUpload was in
  // flight marked as canceled an object that the completion created.
  it('cannot cancel an upload while it is completing', async () => {
    const services = smallPartServices();
    const gate = deferred<void>();
    const original = services.uploads.complete.bind(services.uploads);
    const complete = vi
      .spyOn(services.uploads, 'complete')
      .mockImplementation(async (bucket, key, uploadId, parts) => {
        await gate.promise;
        return original(bucket, key, uploadId, parts);
      });
    const abort = vi.spyOn(services.uploads, 'abort');
    const manager = createUploadManager({ uploads: services.uploads, singleUploadThreshold: KiB });

    const [id = ''] = manager.enqueue('media', [{ key: 'done.bin', file: blob(3 * KiB) }]);
    await until(manager, () => complete.mock.calls.length > 0);
    expect(manager.getSnapshot().items[0]?.status).toBe('completing');
    await manager.cancel(id);
    await manager.cancelAll();
    expect(manager.getSnapshot().items[0]?.status).toBe('completing');
    expect(abort).not.toHaveBeenCalled();

    gate.resolve();
    await until(manager, () => manager.getSnapshot().items[0]?.status === 'completed');
    expect(abort).not.toHaveBeenCalled();
    expect((await services.objects.stat('media', 'done.bin')).size).toBe(3 * KiB);
  });

  it('cancel during the creation aborts once the upload exists', async () => {
    const services = smallPartServices();
    const created = deferred<void>();
    const original = services.uploads.create.bind(services.uploads);
    vi.spyOn(services.uploads, 'create').mockImplementation(async (...args) => {
      await created.promise;
      return original(...args);
    });
    const abort = vi.spyOn(services.uploads, 'abort');
    const manager = createUploadManager({ uploads: services.uploads, singleUploadThreshold: KiB });

    const [id = ''] = manager.enqueue('media', [{ key: 'early.bin', file: blob(2 * KiB) }]);
    await manager.cancel(id);
    expect(abort).not.toHaveBeenCalled();
    created.resolve();
    await until(manager, () => abort.mock.calls.length === 1);
    expect((await services.uploads.list('media')).items).toEqual([]);
  });

  it('clearing a failed multipart upload aborts it on the server', async () => {
    const services = smallPartServices();
    vi.spyOn(services.uploads, 'uploadPart').mockRejectedValue(
      new ApiError({ title: 'Forbidden', status: 403, code: 'access_denied' }),
    );
    const abort = vi.spyOn(services.uploads, 'abort');
    const manager = createUploadManager({
      uploads: services.uploads,
      singleUploadThreshold: KiB,
      retryDelay: () => 0,
    });

    manager.enqueue('media', [
      { key: 'failed.bin', file: blob(2 * KiB) },
      { key: 'small.bin', file: blob(100) },
    ]);
    await until(manager, () => manager.getSnapshot().active === 0);
    expect(manager.getSnapshot().items.map((i) => i.status)).toEqual(['failed', 'completed']);
    expect((await services.uploads.list('media')).items).toHaveLength(1);
    expect(abort).not.toHaveBeenCalled();

    manager.clearFinished();
    expect(manager.getSnapshot().items).toEqual([]);
    await until(manager, () => abort.mock.calls.length === 1);
    expect(abort.mock.calls[0]?.slice(0, 2)).toEqual(['media', 'failed.bin']);
    await until(manager, () => abort.mock.results[0]?.type === 'return');
    await abort.mock.results[0]?.value;
    // No pending upload remains on the server.
    expect((await services.uploads.list('media')).items).toEqual([]);
  });

  it('aggregates the progress of the queue and clears finished files', async () => {
    const services = smallPartServices();
    const manager = createUploadManager({ uploads: services.uploads, singleUploadThreshold: KiB });
    const listener = vi.fn();
    manager.subscribe(listener);

    manager.enqueue('media', [
      { key: 'one.bin', file: blob(500) },
      { key: 'two.bin', file: blob(2 * KiB) },
    ]);
    expect(manager.getSnapshot()).toMatchObject({ totalBytes: 500 + 2 * KiB, active: 2 });
    await until(manager, () => manager.getSnapshot().active === 0);
    expect(manager.getSnapshot().loadedBytes).toBe(500 + 2 * KiB);
    expect(listener).toHaveBeenCalled();

    manager.clearFinished();
    expect(manager.getSnapshot().items).toEqual([]);
  });
});

// covers: D-OBJ-02
describe('upload encryption (D-OBJ-02, SSE per upload)', () => {
  it('asks for the encryption of every file, single or multipart', async () => {
    const services = smallPartServices();
    const put = vi.spyOn(services.uploads, 'putObject');
    const create = vi.spyOn(services.uploads, 'create');
    const manager = createUploadManager({
      uploads: services.uploads,
      singleUploadThreshold: KiB,
    });
    const encryption = { algorithm: 'aws:kms', kmsKeyId: 'nexo-default-key' } as const;
    manager.enqueue('media', [
      { key: 'small.bin', file: blob(10), encryption },
      { key: 'big.bin', file: blob(3 * KiB), encryption },
      { key: 'plain.bin', file: blob(10) },
    ]);
    await until(manager, () => manager.getSnapshot().items.every((i) => i.status === 'completed'));
    expect(put).toHaveBeenCalledWith(
      expect.objectContaining({ key: 'small.bin', encryption }),
      expect.anything(),
    );
    expect(put).toHaveBeenCalledWith(
      expect.not.objectContaining({ encryption: expect.anything() as unknown }),
      expect.anything(),
    );
    expect(create).toHaveBeenCalledWith('media', 'big.bin', {
      contentType: 'application/octet-stream',
      encryption,
    });
    const big = await services.objects.stat('media', 'big.bin');
    expect(big.encryption).toEqual({ type: 'SSE-KMS', kmsKeyId: 'nexo-default-key' });
    const plain = await services.objects.stat('media', 'plain.bin');
    expect(plain.encryption).toBeUndefined();
  });

  it('fails the files when the server has no KMS', async () => {
    const services = createMockServices({ kms: { configured: false, online: false } });
    const manager = createUploadManager({ uploads: services.uploads, maxAttempts: 1 });
    manager.enqueue('media', [
      { key: 'x.bin', file: blob(10), encryption: { algorithm: 'AES256' } },
    ]);
    await until(manager, () => manager.getSnapshot().items[0]?.status === 'failed');
    expect(manager.getSnapshot().items[0]?.errorCode).toBe('not_implemented');
  });
});
