import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { simulateInput } from '@/lib/simulator';
import { createMockServices } from '@/services/mock';
import { renderRoutes } from '@/test/render';

import { PolicySimulatorPage } from './PolicySimulatorPage';

describe('Policy simulator (D-POL-03)', () => {
  it('shows the decision, the deciding statement and every evaluation', async () => {
    const user = userEvent.setup();
    const services = createMockServices({ initialAccessKey: 'nexoadmin' });
    const simulate = vi.spyOn(services.policies, 'simulate');
    renderRoutes([{ path: '/', element: <PolicySimulatorPage /> }], { services });
    const form = await screen.findByRole('form', { name: 'Policy simulator' });
    const userField = within(form).getByRole('textbox', { name: /User or access key/ });
    await user.clear(userField);
    await user.type(userField, 'alice');
    await user.type(within(form).getByRole('textbox', { name: 'Bucket' }), 'media');
    await user.type(within(form).getByRole('textbox', { name: 'Object' }), 'a.txt');
    await user.type(within(form).getByRole('textbox', { name: 'Source IP' }), '10.0.0.1');
    await user.click(within(form).getByRole('radio', { name: 'Yes' }));
    await user.click(within(form).getByRole('button', { name: 'Simulate' }));

    const status = await screen.findByRole('alert');
    expect(status).toHaveTextContent('Allowed');
    expect(status).toHaveTextContent('alice → s3:GetObject on media/a.txt');
    expect(status).toHaveTextContent(/Decided by policy readwrite/);
    expect(simulate).toHaveBeenCalledWith({
      user: 'alice',
      action: 's3:GetObject',
      bucket: 'media',
      object: 'a.txt',
      sourceIp: '10.0.0.1',
      secureTransport: true,
    });
    const table = screen.getByRole('table', { name: 'Evaluated statements' });
    expect(within(table).getAllByText(/Matches/).length).toBeGreaterThan(0);
    expect(within(table).getByText(/deciding/)).toBeInTheDocument();
  });

  it('explains an implicit deny', async () => {
    const user = userEvent.setup();
    const services = createMockServices({ initialAccessKey: 'nexoadmin' });
    renderRoutes([{ path: '/', element: <PolicySimulatorPage /> }], { services });
    const form = await screen.findByRole('form', { name: 'Policy simulator' });
    const userField = within(form).getByRole('textbox', { name: /User or access key/ });
    await user.clear(userField);
    await user.type(userField, 'ci-bot');
    const action = within(form).getByRole('textbox', { name: /Action/ });
    await user.clear(action);
    await user.type(action, 'admin:ListUsers');
    await user.click(within(form).getByRole('button', { name: 'Simulate' }));
    const status = await screen.findByRole('alert');
    expect(status).toHaveTextContent('Denied');
    expect(status).toHaveTextContent(/implicit deny/);
    expect(screen.getAllByText(/No match \(action\)/).length).toBeGreaterThan(0);
  });

  it('builds the request from the form', () => {
    expect(
      simulateInput({
        user: ' ',
        action: 's3:PutObject',
        bucket: '',
        object: 'ignored-without-bucket',
        sourceIp: '',
        tls: 'false',
        time: new Date('2026-10-08T12:00:00Z'),
        conditions: [
          { id: 1, key: 's3:prefix', value: 'a/' },
          { id: 2, key: 's3:prefix', value: 'b/' },
          { id: 3, key: ' ', value: 'x' },
        ],
      }),
    ).toEqual({
      action: 's3:PutObject',
      object: 'ignored-without-bucket',
      secureTransport: false,
      time: '2026-10-08T12:00:00.000Z',
      conditions: { 's3:prefix': ['a/', 'b/'] },
    });
  });
});
