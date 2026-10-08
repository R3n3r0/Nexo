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
```

`npm ci` runs `scripts/go-ignore-node-modules.mjs` (postinstall): it writes a placeholder
`go.mod` into `node_modules/` so that `go build ./...`, `go vet`, `go test` and
`golangci-lint` from the repository root skip the Go sources some npm packages ship.
