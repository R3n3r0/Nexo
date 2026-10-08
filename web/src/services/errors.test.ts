import { describe, expect, it } from 'vitest';

import { ApiError, ClientErrorCode, isApiError } from './errors';

describe('ApiError', () => {
  it('keeps the problem fields and derives the message from detail', () => {
    const err = new ApiError({
      title: 'Not Found',
      status: 404,
      code: 'bucket_not_found',
      detail: 'Bucket x does not exist.',
      requestId: 'r1',
    });
    expect(err.message).toBe('Bucket x does not exist.');
    expect(err.code).toBe('bucket_not_found');
    expect(err.isNotFound).toBe(true);
    expect(err.requestId).toBe('r1');
    expect(isApiError(err)).toBe(true);
    expect(isApiError(new Error('x'))).toBe(false);
  });

  it('fills defaults from a partial problem document', () => {
    const err = ApiError.fromProblem({ code: 'conflict' }, 409);
    expect(err.status).toBe(409);
    expect(err.title).toBe('Conflict');
    expect(err.message).toBe('Conflict');
    expect(err.isConflict).toBe(true);
  });

  it('builds status-only and network errors', () => {
    expect(ApiError.fromStatus(502, 'Bad Gateway').code).toBe(ClientErrorCode.Http);
    expect(ApiError.fromStatus(418, '').title).toBe('HTTP 418');
    const net = ApiError.network(new TypeError('Failed to fetch'));
    expect(net.isNetwork).toBe(true);
    expect(net.detail).toBe('Failed to fetch');
  });
});
