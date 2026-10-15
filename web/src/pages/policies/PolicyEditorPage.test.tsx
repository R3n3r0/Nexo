import { fireEvent, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import type { RouteObject } from 'react-router';
import { describe, expect, it, vi } from 'vitest';

import { ApiError } from '@/services';
import { createMockServices } from '@/services/mock';
import { renderRoutes } from '@/test/render';

import { PolicyEditorPage } from './PolicyEditorPage';

const routes: RouteObject[] = [
  { path: '/policies', element: <div>policy list</div> },
  { path: '/policies/new', element: <PolicyEditorPage /> },
  { path: '/policies/:name', element: <PolicyEditorPage /> },
];

function renderEditor(path = '/policies/new') {
  const services = createMockServices({ initialAccessKey: 'nexoadmin' });
  const view = renderRoutes(routes, { initialEntries: [path], services });
  return { ...view, services };
}

const editor = () => screen.getByRole('textbox', { name: 'Policy document' });
/** Values shown as pills by the MultiSelect of the builder. */
const pills = () =>
  [...document.querySelectorAll('.mantine-Pill-label')].map((el) => el.textContent ?? '');
const setText = (value: string) => {
  fireEvent.change(editor(), { target: { value } });
};

const validPolicy = JSON.stringify(
  {
    Version: '2012-10-17',
    Statement: [{ Effect: 'Allow', Action: ['s3:GetObject'], Resource: ['arn:aws:s3:::photos/*'] }],
  },
  null,
  2,
);

describe('Policy editor (TD-U-07)', () => {
  it('highlights invalid JSON with its position and blocks saving', async () => {
    renderEditor();
    await screen.findByRole('textbox', { name: 'Policy document' });
    setText('{\n  "Version": "2012-10-17",\n  "Statement": [\n');
    expect(screen.getByRole('alert')).toHaveTextContent('Invalid JSON at line 4, column 1');
    const problems = screen.getByRole('list', { name: 'Problems' });
    expect(problems).toHaveTextContent('Line 4, column 1');
    expect(editor()).toHaveAttribute('aria-invalid', 'true');
    expect(screen.getByRole('button', { name: 'Save' })).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Validate' })).toBeDisabled();
  });

  it('lints the document while typing (unknown action at its position)', async () => {
    renderEditor();
    await screen.findByRole('button', { name: 'Validate' });
    setText(validPolicy.replace('s3:GetObject', 's3:Teleport'));
    expect(screen.getByRole('list', { name: 'Problems' })).toHaveTextContent(
      'Line 7, column 9: Action: unknown S3 action "s3:Teleport"',
    );
  });

  it('shows the validation errors answered by the server', async () => {
    const user = userEvent.setup();
    const { services } = renderEditor();
    vi.spyOn(services.policies, 'validate').mockResolvedValue({
      valid: false,
      errors: [
        { statement: 1, element: 'Resource', line: 7, column: 9, message: 'bucket is reserved' },
      ],
    });
    await screen.findByRole('button', { name: 'Validate' });
    setText(validPolicy);
    await user.click(screen.getByRole('button', { name: 'Validate' }));
    expect(await screen.findByText(/Resource: bucket is reserved/)).toBeInTheDocument();
    expect(screen.getByRole('list', { name: 'Problems' })).toHaveTextContent('Line 7, column 9');
    // Editing the text discards the answer about the previous text.
    setText(`${validPolicy}\n`);
    expect(screen.queryByText(/bucket is reserved/)).not.toBeInTheDocument();
  });

  it('shows the located errors of a rejected save', async () => {
    const user = userEvent.setup();
    const { services } = renderEditor();
    vi.spyOn(services.policies, 'create').mockRejectedValue(
      new ApiError({
        title: 'Bad Request',
        status: 400,
        code: 'malformed_policy',
        errors: [{ statement: 1, line: 3, column: 3, message: 'statement rejected by the server' }],
      }),
    );
    await user.type(await screen.findByRole('textbox', { name: 'Name' }), 'photos-read');
    setText(validPolicy);
    await user.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByText(/The policy has 1 problem:/)).toBeInTheDocument();
    expect(screen.getByRole('list', { name: 'Problems' })).toHaveTextContent(
      'Line 3, column 3: statement rejected by the server',
    );
  });

  // Many interactions with the builder and the editor: a loaded machine
  // (the Go race tests running alongside) slows them past the default 5 s.
  it('keeps the visual builder and the JSON in sync', { timeout: 30_000 }, async () => {
    const user = userEvent.setup();
    renderEditor();
    await screen.findByRole('textbox', { name: 'Policy document' });
    setText(validPolicy);

    await user.click(screen.getByRole('tab', { name: 'Visual builder' }));
    expect(pills()).toContain('s3:GetObject');
    await user.click(screen.getByRole('radio', { name: 'Deny' }));
    await user.type(screen.getByRole('textbox', { name: 'Bucket' }), 'archive');
    await user.type(screen.getByRole('textbox', { name: 'Prefix' }), '2026/');
    await user.click(screen.getByRole('button', { name: 'Add bucket and prefix' }));

    await user.click(screen.getByRole('tab', { name: 'JSON' }));
    const json = JSON.parse((editor() as HTMLTextAreaElement).value) as {
      Statement: Array<{ Effect: string; Resource: string[] }>;
    };
    expect(json.Statement[0]?.Effect).toBe('Deny');
    expect(json.Statement[0]?.Resource).toEqual([
      'arn:aws:s3:::photos/*',
      'arn:aws:s3:::archive',
      'arn:aws:s3:::archive/2026/*',
    ]);

    // And back: a JSON edit shows up in the builder.
    setText(validPolicy.replace('s3:GetObject', 's3:PutObject'));
    await user.click(screen.getByRole('tab', { name: 'Visual builder' }));
    expect(pills()).toContain('s3:PutObject');
    expect(pills()).not.toContain('s3:GetObject');
    await user.click(screen.getByRole('button', { name: 'Add statement' }));
    expect(screen.getByRole('region', { name: 'Statement 2' })).toBeInTheDocument();
  });

  it('asks to fix the JSON before using the builder', async () => {
    const user = userEvent.setup();
    renderEditor();
    await screen.findByRole('textbox', { name: 'Policy document' });
    setText('{');
    await user.click(screen.getByRole('tab', { name: 'Visual builder' }));
    expect(
      screen.getByText('Fix the JSON document to use the visual builder.'),
    ).toBeInTheDocument();
  });

  it('creates a policy and opens it', async () => {
    const user = userEvent.setup();
    const { router, services } = renderEditor();
    await user.type(await screen.findByRole('textbox', { name: 'Name' }), 'photos-read');
    setText(validPolicy);
    await user.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/policies/photos-read');
    });
    expect((await services.policies.get('photos-read')).document.Statement).toHaveLength(1);
  });

  it('shows built-in policies read-only with their entities', async () => {
    renderEditor('/policies/readwrite');
    expect(await screen.findByText(/Built-in policies cannot be changed/)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Save' })).not.toBeInTheDocument();
    expect(editor()).toHaveAttribute('readonly');
    const entities = await screen.findByRole('region', { name: 'Attached to' });
    await waitFor(() => {
      expect(entities).toHaveTextContent('alice');
    });
    expect(entities).toHaveTextContent('developers');
  });
});
