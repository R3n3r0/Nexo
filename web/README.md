# Nexo console

React dashboard served by the `nexo` binary on `:9001`.

- Vite 6, React 19, TypeScript (strict), React Router v7, TanStack Query v5, Mantine v7,
  i18next (it, en), Vitest + Testing Library + MSW.
- Services follow docs/12 §12.5: `src/services/interfaces.ts` declares the contracts,
  `src/services/http/` implements them against the Admin API (`/api/v1`),
  `src/services/mock/` implements them in memory (tests and demo mode). Components only
  use `useServices()`.
- The wire types (`src/services/types.ts`) derive from the OpenAPI contract
  `api/openapi.yaml`: `npm run gen:api` regenerates `src/services/http/schema.d.ts`
  (openapi-typescript) and CI fails when the generated file is stale.

```sh
npm ci
npm run dev            # http://localhost:5173, proxies /api and /nexo to :9001
VITE_DEMO=1 npm run dev  # demo mode with mock services (nexoadmin / nexoadmin)
npm run lint && npm run typecheck && npm test -- --run
npm run gen:api        # regenerate src/services/http/schema.d.ts from ../api/openapi.yaml
npm run build          # output in dist/, embedded with: go build -tags embedui ./...
npm run size           # after the build: initial bundle <= 300 KiB gzip (size-limit, docs/06 §6.5)
```

`npm ci` runs `scripts/go-ignore-node-modules.mjs` (postinstall): it writes a placeholder
`go.mod` into `node_modules/` so that `go build ./...`, `go vet`, `go test` and
`golangci-lint` from the repository root skip the Go sources some npm packages ship.

## End-to-end tests (Playwright)

`npm run e2e` runs `e2e/*.spec.ts` (docs/06 §6.5 TD-E/TD-S, docs/09 §9.6 TD-P) in Chromium
against the real `bin/nexo` with the embedded dashboard:

- `e2e/global-setup.ts` rebuilds the binary when it is missing or older than its sources
  (`npm run build` + `make build-embedded`; set `NEXO_E2E_BINARY` to use another one), starts
  it on two free loopback ports with a temporary drive and test root credentials, creates the
  test users through the Admin API (SigV4) and saves a root console session;
- `e2e/global-teardown.ts` stops the server by PID and removes the temporary directory
  (`NEXO_E2E_KEEP=1` keeps it, with `nexo.log`);
- the specs reach the server through the fixtures of `e2e/support/fixtures.ts` (`api`: signed
  Admin/S3 client, `nexo`: ports and credentials).

In CI, `make build` runs first and `npx playwright install --with-deps chromium` provides the
browser; locally the Chromium of `PLAYWRIGHT_BROWSERS_PATH` is used (or
`NEXO_E2E_CHROMIUM=/path/to/chrome`).
