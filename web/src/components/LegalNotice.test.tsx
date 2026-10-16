import { screen, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { renderWithProviders } from '@/test/render';

import { LegalNotice } from './LegalNotice';

describe('LegalNotice', () => {
  it('shows the license, the attribution and the source code (AGPL-3.0 §13)', () => {
    renderWithProviders(<LegalNotice version="v1.2.3" commit="abc1234" />);
    const notice = screen.getByRole('contentinfo', { name: 'License and source code' });
    expect(notice).toHaveTextContent('Nexo v1.2.3');
    expect(within(notice).getByRole('link', { name: 'AGPL-3.0' })).toHaveAttribute(
      'href',
      'https://www.gnu.org/licenses/agpl-3.0.html',
    );
    expect(within(notice).getByRole('link', { name: 'Powered by Nexo' })).toHaveAttribute(
      'href',
      'https://github.com/R3n3r0/Nexo',
    );
    expect(within(notice).getByRole('link', { name: 'Source code' })).toHaveAttribute(
      'href',
      'https://github.com/R3n3r0/Nexo/tree/v1.2.3',
    );
  });

  it('links the repository without a version', () => {
    renderWithProviders(<LegalNotice />);
    const notice = screen.getByTestId('legal-notice');
    expect(notice).toHaveTextContent(/^Nexo/);
    expect(within(notice).getByRole('link', { name: 'Source code' })).toHaveAttribute(
      'href',
      'https://github.com/R3n3r0/Nexo',
    );
  });
});
