import { describe, expect, it } from 'vitest';

import { license, sourceRepository, sourceUrl } from './legal';

describe('legal notices', () => {
  it('declares the AGPL and the repository', () => {
    expect(license).toBe('AGPL-3.0-only');
    expect(sourceRepository).toBe('https://github.com/R3n3r0/Nexo');
  });

  it('links the source code of the build', () => {
    expect(sourceUrl('v1.0.0', 'abc1234')).toBe(`${sourceRepository}/tree/v1.0.0`);
    expect(sourceUrl('v1.0.0-rc.1')).toBe(`${sourceRepository}/tree/v1.0.0-rc.1`);
    expect(sourceUrl('v1.0.0-3-gabc1234-dirty', 'abc1234')).toBe(
      `${sourceRepository}/tree/abc1234`,
    );
    expect(sourceUrl('dev', 'none')).toBe(sourceRepository);
    expect(sourceUrl()).toBe(sourceRepository);
  });
});
