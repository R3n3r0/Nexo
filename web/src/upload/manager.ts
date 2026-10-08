import { isApiError, type CompletedPart, type Upload, type UploadService } from '@/services';

import { FileTooLargeError, partRanges, planParts, type PartRange } from './plan';

/** Lifecycle of a file in the upload queue. */
export type UploadStatus =
  'queued' | 'uploading' | 'paused' | 'completing' | 'completed' | 'failed' | 'canceled';

/** A file to upload: its destination key and its content. */
export interface UploadSource {
  key: string;
  file: Blob;
  /** Defaults to the type of the blob. */
  contentType?: string;
}

/** Immutable view of a queued file, for the UI. */
export interface UploadItem {
  readonly id: string;
  readonly bucket: string;
  readonly key: string;
  readonly size: number;
  readonly status: UploadStatus;
  /** Bytes acknowledged plus bytes of the requests in flight. */
  readonly loaded: number;
  readonly multipart: boolean;
  /** Number of parts (1 for a single-request upload). */
  readonly parts: number;
  readonly partsDone: number;
  /** Message and code of the last failure. */
  readonly error?: string;
  readonly errorCode?: string;
}

/** Immutable state of the whole queue. */
export interface UploadSnapshot {
  readonly items: readonly UploadItem[];
  /** Bytes of the files not canceled. */
  readonly totalBytes: number;
  readonly loadedBytes: number;
  /** Files queued, uploading, paused or completing. */
  readonly active: number;
}

export interface UploadManagerOptions {
  uploads: UploadService;
  /** Files up to this size go in one request (default 16 MiB). */
  singleUploadThreshold?: number;
  /** Requests in flight across the whole queue (default 4). */
  concurrency?: number;
  /** Attempts of a part (or single upload) before the file fails (default 3). */
  maxAttempts?: number;
  /** Delay before the n-th retry (default 500 ms × 2^(n-1)). */
  retryDelay?: (attempt: number) => number;
  /** Timer used for the retries (tests). */
  setTimer?: (fn: () => void, ms: number) => unknown;
}

/**
 * The upload queue of the console: multipart uploads proxied by the Admin
 * API with a bounded number of requests in flight, automatic retry of a
 * failed part, pause/resume/cancel per file (cancel aborts the multipart
 * upload on the server) and aggregated progress. Framework-agnostic: the
 * UI subscribes to it with useSyncExternalStore.
 */
export interface UploadManager {
  /** Queues files for a bucket; returns their ids. */
  enqueue(bucket: string, sources: readonly UploadSource[]): string[];
  /** Stops the requests of a file; the parts acknowledged are kept. */
  pause(id: string): void;
  resume(id: string): void;
  /** Stops a file and aborts its multipart upload on the server. */
  cancel(id: string): Promise<void>;
  /** Re-queues a failed file: only the missing parts are sent again. */
  retry(id: string): void;
  pauseAll(): void;
  resumeAll(): void;
  cancelAll(): Promise<void>;
  /** Removes completed, failed and canceled files from the list. */
  clearFinished(): void;
  /** Store contract of useSyncExternalStore (arrow properties: safe to pass unbound). */
  subscribe: (listener: () => void) => () => void;
  getSnapshot: () => UploadSnapshot;
  /** Called once per file stored on the server. */
  onCompleted(listener: (item: UploadItem) => void): () => void;
  /** Cancels every file (sign-out). The manager stays usable. */
  dispose(): Promise<void>;
}

interface PartState extends PartRange {
  status: 'pending' | 'uploading' | 'done';
  attempts: number;
  loaded: number;
  etag?: string;
  controller?: AbortController;
  /** A retry is scheduled: not eligible until the timer fires. */
  waiting: boolean;
}

interface Task {
  id: string;
  bucket: string;
  key: string;
  blob: Blob;
  contentType: string;
  status: UploadStatus;
  multipart: boolean;
  upload?: Upload;
  creating: boolean;
  completing: boolean;
  parts: PartState[];
  /** Single-request upload state. */
  single: {
    status: 'pending' | 'uploading' | 'done';
    attempts: number;
    loaded: number;
    waiting: boolean;
    controller?: AbortController;
  };
  error?: string;
  errorCode?: string;
}

const MiB = 1024 * 1024;

const activeStatuses: ReadonlySet<UploadStatus> = new Set([
  'queued',
  'uploading',
  'paused',
  'completing',
]);

/** Errors worth retrying: network failures, timeouts, throttling, 5xx. */
function isTransient(error: unknown): boolean {
  if (!isApiError(error)) {
    return true;
  }
  if (error.isAborted) {
    return false;
  }
  return error.status === 0 || error.status === 408 || error.status === 429 || error.status >= 500;
}

function describeError(error: unknown): { message: string; code?: string } {
  if (isApiError(error)) {
    return { message: error.message, code: error.code };
  }
  if (error instanceof FileTooLargeError) {
    return { message: error.message, code: 'entity_too_large' };
  }
  return { message: error instanceof Error ? error.message : String(error) };
}

export function createUploadManager(options: UploadManagerOptions): UploadManager {
  const service = options.uploads;
  const threshold = options.singleUploadThreshold ?? 16 * MiB;
  const concurrency = Math.max(1, options.concurrency ?? 4);
  const maxAttempts = Math.max(1, options.maxAttempts ?? 3);
  const retryDelay = options.retryDelay ?? ((n: number) => 500 * 2 ** (n - 1));
  const setTimer = options.setTimer ?? ((fn: () => void, ms: number) => setTimeout(fn, ms));

  const tasks: Task[] = [];
  const listeners = new Set<() => void>();
  const completedListeners = new Set<(item: UploadItem) => void>();
  let inFlight = 0;
  let counter = 0;
  let snapshot: UploadSnapshot | null = null;
  let notifyScheduled = false;

  function find(id: string): Task | undefined {
    return tasks.find((t) => t.id === id);
  }

  function loadedOf(task: Task): number {
    if (task.status === 'completed') {
      return task.blob.size;
    }
    if (!task.multipart) {
      return task.single.loaded;
    }
    return task.parts.reduce(
      (sum, p) =>
        sum + (p.status === 'done' ? p.end - p.start : p.status === 'uploading' ? p.loaded : 0),
      0,
    );
  }

  function itemOf(task: Task): UploadItem {
    const partsDone = task.multipart
      ? task.parts.filter((p) => p.status === 'done').length
      : task.single.status === 'done' || task.status === 'completed'
        ? 1
        : 0;
    return {
      id: task.id,
      bucket: task.bucket,
      key: task.key,
      size: task.blob.size,
      status: task.status,
      loaded: Math.min(task.blob.size, loadedOf(task)),
      multipart: task.multipart,
      parts: task.multipart ? Math.max(task.parts.length, 1) : 1,
      partsDone,
      ...(task.error !== undefined ? { error: task.error } : {}),
      ...(task.errorCode !== undefined ? { errorCode: task.errorCode } : {}),
    };
  }

  function changed() {
    snapshot = null;
    if (notifyScheduled) {
      return;
    }
    notifyScheduled = true;
    queueMicrotask(() => {
      notifyScheduled = false;
      for (const listener of [...listeners]) {
        listener();
      }
    });
  }

  function fail(task: Task, error: unknown) {
    const { message, code } = describeError(error);
    task.status = 'failed';
    task.error = message;
    if (code !== undefined) {
      task.errorCode = code;
    } else {
      delete task.errorCode;
    }
    stopRequests(task);
    changed();
  }

  /** Aborts the requests of a task; their parts go back to pending. */
  function stopRequests(task: Task) {
    for (const part of task.parts) {
      if (part.status === 'uploading') {
        part.controller?.abort();
        part.status = 'pending';
        part.loaded = 0;
      }
    }
    if (task.single.status === 'uploading') {
      task.single.controller?.abort();
      task.single.status = 'pending';
      task.single.loaded = 0;
    }
  }

  function runnable(task: Task): boolean {
    return task.status === 'queued' || task.status === 'uploading';
  }

  /** Starts as many requests as the concurrency allows, in queue order. */
  function pump() {
    for (const task of tasks) {
      if (inFlight >= concurrency) {
        return;
      }
      if (!runnable(task)) {
        continue;
      }
      if (!task.multipart) {
        if (task.single.status === 'pending' && !task.single.waiting) {
          startSingle(task);
        }
        continue;
      }
      if (task.upload === undefined) {
        if (!task.creating) {
          startCreate(task);
        }
        continue;
      }
      for (const part of task.parts) {
        if (inFlight >= concurrency) {
          return;
        }
        if (part.status === 'pending' && !part.waiting) {
          startPart(task, part);
        }
      }
      maybeComplete(task);
    }
  }

  function begin(task: Task) {
    if (task.status === 'queued') {
      task.status = 'uploading';
    }
    inFlight += 1;
    changed();
  }

  function end() {
    inFlight -= 1;
    changed();
    pump();
  }

  /** Schedules another attempt, or fails the task once attempts run out. */
  function retryLater(task: Task, unit: { attempts: number; waiting: boolean }, error: unknown) {
    if (!isTransient(error) || unit.attempts >= maxAttempts) {
      fail(task, error);
      return;
    }
    unit.waiting = true;
    setTimer(() => {
      unit.waiting = false;
      pump();
    }, retryDelay(unit.attempts));
  }

  function startSingle(task: Task) {
    const controller = new AbortController();
    task.single = { ...task.single, status: 'uploading', loaded: 0, controller };
    task.single.attempts += 1;
    begin(task);
    service
      .putObject(
        { bucket: task.bucket, key: task.key, body: task.blob, contentType: task.contentType },
        {
          signal: controller.signal,
          onProgress: (loaded) => {
            if (task.single.controller === controller) {
              task.single.loaded = loaded;
              changed();
            }
          },
        },
      )
      .then(
        () => {
          if (task.single.controller !== controller) {
            return;
          }
          task.single.status = 'done';
          task.single.loaded = task.blob.size;
          finish(task);
        },
        (error: unknown) => {
          if (task.single.controller !== controller || controller.signal.aborted) {
            return;
          }
          task.single.status = 'pending';
          task.single.loaded = 0;
          retryLater(task, task.single, error);
        },
      )
      .finally(end);
  }

  function startCreate(task: Task) {
    task.creating = true;
    begin(task);
    service
      .create(task.bucket, task.key, { contentType: task.contentType })
      .then(
        (upload) => {
          task.upload = upload;
          if (task.status === 'canceled') {
            void abortOnServer(task);
            return;
          }
          try {
            const plan = planParts(task.blob.size, upload);
            task.parts = partRanges(task.blob.size, plan).map((r) => ({
              ...r,
              status: 'pending',
              attempts: 0,
              loaded: 0,
              waiting: false,
            }));
          } catch (error) {
            fail(task, error);
            void abortOnServer(task);
          }
        },
        (error: unknown) => {
          if (task.status !== 'canceled') {
            fail(task, error);
          }
        },
      )
      .finally(() => {
        task.creating = false;
        end();
      });
  }

  function startPart(task: Task, part: PartState) {
    const upload = task.upload;
    if (!upload) {
      return;
    }
    const controller = new AbortController();
    part.status = 'uploading';
    part.loaded = 0;
    part.controller = controller;
    part.attempts += 1;
    begin(task);
    service
      .uploadPart(
        {
          bucket: task.bucket,
          key: task.key,
          uploadId: upload.uploadId,
          partNumber: part.partNumber,
          body: task.blob.slice(part.start, part.end),
        },
        {
          signal: controller.signal,
          onProgress: (loaded) => {
            if (part.controller === controller && part.status === 'uploading') {
              part.loaded = loaded;
              changed();
            }
          },
        },
      )
      .then(
        (result) => {
          if (part.controller !== controller || part.status !== 'uploading') {
            return;
          }
          part.status = 'done';
          part.etag = result.etag;
          delete part.controller;
          maybeComplete(task);
        },
        (error: unknown) => {
          if (part.controller !== controller || controller.signal.aborted) {
            return;
          }
          part.status = 'pending';
          part.loaded = 0;
          delete part.controller;
          if (runnable(task)) {
            retryLater(task, part, error);
          }
        },
      )
      .finally(end);
  }

  function maybeComplete(task: Task) {
    const upload = task.upload;
    if (
      !upload ||
      !runnable(task) ||
      task.completing ||
      task.parts.length === 0 ||
      task.parts.some((p) => p.status !== 'done')
    ) {
      return;
    }
    task.completing = true;
    task.status = 'completing';
    changed();
    const parts: CompletedPart[] = task.parts.map((p) => ({
      partNumber: p.partNumber,
      etag: p.etag ?? '',
    }));
    service.complete(task.bucket, task.key, upload.uploadId, parts).then(
      () => {
        task.completing = false;
        if (task.status === 'completing') {
          finish(task);
        }
      },
      (error: unknown) => {
        task.completing = false;
        if (task.status === 'completing') {
          fail(task, error);
        }
      },
    );
  }

  function finish(task: Task) {
    task.status = 'completed';
    delete task.error;
    delete task.errorCode;
    changed();
    const item = itemOf(task);
    for (const listener of [...completedListeners]) {
      listener(item);
    }
  }

  async function abortOnServer(task: Task) {
    const upload = task.upload;
    if (!upload) {
      return;
    }
    try {
      await service.abort(task.bucket, task.key, upload.uploadId);
    } catch (error) {
      // An upload already gone is fine; anything else stays pending on the
      // server until the lifecycle scanner removes it.
      if (!(isApiError(error) && error.isNotFound)) {
        console.warn('upload: abort failed', task.key, error);
      }
    }
  }

  const manager: UploadManager = {
    enqueue(bucket, sources) {
      const ids: string[] = [];
      for (const source of sources) {
        counter += 1;
        const id = `upload-${String(counter)}`;
        const type = source.contentType ?? source.file.type;
        tasks.push({
          id,
          bucket,
          key: source.key,
          blob: source.file,
          contentType: type !== '' ? type : 'application/octet-stream',
          status: 'queued',
          multipart: source.file.size > threshold,
          creating: false,
          completing: false,
          parts: [],
          single: { status: 'pending', attempts: 0, loaded: 0, waiting: false },
        });
        ids.push(id);
      }
      changed();
      pump();
      return ids;
    },
    pause(id) {
      const task = find(id);
      if (!task || (task.status !== 'queued' && task.status !== 'uploading')) {
        return;
      }
      task.status = 'paused';
      stopRequests(task);
      changed();
    },
    resume(id) {
      const task = find(id);
      if (!task || task.status !== 'paused') {
        return;
      }
      task.status = 'queued';
      changed();
      pump();
    },
    async cancel(id) {
      const task = find(id);
      if (!task || (!activeStatuses.has(task.status) && task.status !== 'failed')) {
        return;
      }
      if (task.completing || task.status === 'completing') {
        // CompleteMultipartUpload is in flight: the object is (about to
        // be) created, an abort would only fail with NoSuchUpload and the
        // queue would claim a canceled upload that exists. The outcome of
        // the completion decides.
        return;
      }
      task.status = 'canceled';
      stopRequests(task);
      changed();
      pump();
      // When the creation is still in flight, its callback aborts.
      if (!task.creating) {
        await abortOnServer(task);
      }
    },
    retry(id) {
      const task = find(id);
      if (!task || task.status !== 'failed') {
        return;
      }
      task.status = 'queued';
      delete task.error;
      delete task.errorCode;
      for (const part of task.parts) {
        part.attempts = 0;
      }
      task.single.attempts = 0;
      changed();
      pump();
      maybeComplete(task);
    },
    pauseAll() {
      for (const task of tasks) {
        manager.pause(task.id);
      }
    },
    resumeAll() {
      for (const task of tasks) {
        manager.resume(task.id);
      }
    },
    async cancelAll() {
      await Promise.all(tasks.map((t) => manager.cancel(t.id)));
    },
    clearFinished() {
      for (let i = tasks.length - 1; i >= 0; i -= 1) {
        const task = tasks[i];
        const status = task?.status;
        if (status === 'completed' || status === 'failed' || status === 'canceled') {
          // A failed multipart upload can no longer be retried once it is
          // cleared: abort it, or its parts stay pending on the server.
          if (task && status === 'failed') {
            void abortOnServer(task);
          }
          tasks.splice(i, 1);
        }
      }
      changed();
    },
    subscribe(listener) {
      listeners.add(listener);
      return () => {
        listeners.delete(listener);
      };
    },
    getSnapshot() {
      if (snapshot === null) {
        const items = tasks.map(itemOf);
        let totalBytes = 0;
        let loadedBytes = 0;
        let active = 0;
        for (const item of items) {
          if (item.status !== 'canceled') {
            totalBytes += item.size;
            loadedBytes += item.loaded;
          }
          if (activeStatuses.has(item.status)) {
            active += 1;
          }
        }
        snapshot = { items, totalBytes, loadedBytes, active };
      }
      return snapshot;
    },
    onCompleted(listener) {
      completedListeners.add(listener);
      return () => {
        completedListeners.delete(listener);
      };
    },
    async dispose() {
      await manager.cancelAll();
    },
  };
  return manager;
}
