import { describe, expect, it } from 'vitest';

import { redirectTargetFromState, sanitizeRedirect } from './redirect';

describe('redirectTargetFromState', () => {
  it('returns the requested internal route', () => {
    expect(redirectTargetFromState({ from: '/buckets?x=1' })).toBe('/buckets?x=1');
  });

  it('falls back to the overview for missing or malformed state', () => {
    expect(redirectTargetFromState(null)).toBe('/');
    expect(redirectTargetFromState(undefined)).toBe('/');
    expect(redirectTargetFromState({ from: 42 })).toBe('/');
    expect(redirectTargetFromState('/')).toBe('/');
  });
});

describe('sanitizeRedirect', () => {
  it('rejects external and protocol-relative targets', () => {
    expect(sanitizeRedirect('https://evil.example')).toBe('/');
    expect(sanitizeRedirect('//evil.example/path')).toBe('/');
    expect(sanitizeRedirect('/\\evil.example')).toBe('/');
    expect(sanitizeRedirect('buckets')).toBe('/');
  });

  it('never loops back to the login page', () => {
    expect(sanitizeRedirect('/login')).toBe('/');
    expect(sanitizeRedirect('/login?next=x')).toBe('/');
    expect(sanitizeRedirect('/login/other')).toBe('/');
    expect(sanitizeRedirect('/loginish')).toBe('/loginish');
  });
});
