import js from '@eslint/js';
import prettier from 'eslint-config-prettier';
import reactHooks from 'eslint-plugin-react-hooks';
import reactRefresh from 'eslint-plugin-react-refresh';
import globals from 'globals';
import tseslint from 'typescript-eslint';

// Files allowed to talk to the network directly. Everything else must go
// through the services obtained from useServices() (docs/12 §12.5).
const networkLayer = ['src/services/http/**', 'src/test/**', 'src/**/*.test.{ts,tsx}'];

// Files allowed to import concrete service implementations. Components and
// pages only ever see the interfaces, through the ServicesProvider.
const compositionLayer = ['src/services/**', 'src/test/**', 'src/**/*.test.{ts,tsx}'];

export default tseslint.config(
  {
    ignores: [
      'dist/**',
      'coverage/**',
      'node_modules/**',
      'playwright-report/**',
      // Generated from api/openapi.yaml by `npm run gen:api`.
      'src/services/http/schema.d.ts',
    ],
  },

  // TypeScript sources (app + vite config).
  {
    files: ['**/*.{ts,tsx}'],
    extends: [js.configs.recommended, ...tseslint.configs.recommendedTypeChecked, prettier],
    languageOptions: {
      ecmaVersion: 2022,
      globals: { ...globals.browser, ...globals.es2021 },
      parserOptions: {
        projectService: true,
        tsconfigRootDir: import.meta.dirname,
      },
    },
    plugins: {
      'react-hooks': reactHooks,
      'react-refresh': reactRefresh,
    },
    rules: {
      ...reactHooks.configs['recommended-latest'].rules,
      'react-refresh/only-export-components': ['warn', { allowConstantExport: true }],
      '@typescript-eslint/consistent-type-imports': [
        'error',
        { prefer: 'type-imports', fixStyle: 'inline-type-imports' },
      ],
      '@typescript-eslint/no-unused-vars': [
        'error',
        { argsIgnorePattern: '^_', varsIgnorePattern: '^_', caughtErrorsIgnorePattern: '^_' },
      ],
      '@typescript-eslint/no-misused-promises': [
        'error',
        { checksVoidReturn: { attributes: false } },
      ],
      '@typescript-eslint/restrict-template-expressions': ['error', { allowNumber: true }],
      eqeqeq: ['error', 'always'],
      'no-console': ['warn', { allow: ['warn', 'error'] }],
    },
  },

  // Components never call fetch directly: the HTTP layer is the only place
  // that knows about the wire format.
  {
    files: ['src/**/*.{ts,tsx}'],
    ignores: networkLayer,
    rules: {
      'no-restricted-globals': [
        'error',
        {
          name: 'fetch',
          message: 'Use the services from useServices(); fetch belongs to src/services/http.',
        },
      ],
      'no-restricted-properties': [
        'error',
        { object: 'window', property: 'fetch', message: 'Use the services from useServices().' },
        {
          object: 'globalThis',
          property: 'fetch',
          message: 'Use the services from useServices().',
        },
      ],
    },
  },

  // Components depend on the interfaces only, never on an implementation.
  {
    files: ['src/**/*.{ts,tsx}'],
    ignores: compositionLayer,
    rules: {
      'no-restricted-imports': [
        'error',
        {
          patterns: [
            {
              group: [
                '**/services/http',
                '**/services/http/*',
                '**/services/mock',
                '**/services/mock/*',
              ],
              message: 'Import the service interfaces and read implementations from useServices().',
            },
          ],
        },
      ],
    },
  },

  // Test helpers export fixtures next to wrapper components.
  {
    files: ['src/test/**/*.{ts,tsx}', 'src/**/*.test.{ts,tsx}'],
    rules: { 'react-refresh/only-export-components': 'off' },
  },

  // Plain JS config files.
  {
    files: ['**/*.js', '**/*.mjs'],
    extends: [js.configs.recommended, prettier],
    languageOptions: { ecmaVersion: 2022, sourceType: 'module', globals: globals.node },
  },
  {
    files: ['**/*.cjs'],
    extends: [js.configs.recommended, prettier],
    languageOptions: { ecmaVersion: 2022, sourceType: 'commonjs', globals: globals.node },
  },
);
