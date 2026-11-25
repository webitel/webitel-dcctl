# dcctl UI

Vue 3 + `@webitel/ui-sdk` 26.10. Served by the dcctl binary under `/dcctl/`.

```sh
npm ci
npm run dev           # dev server on the msw mock, no backend needed
npm run dev:backend   # dev server proxying /dcctl/api to a running dcctl
npm run build         # type check + production build into dist/
npm run test          # vitest
npm run lint          # biome
npm run gen:api       # regenerate src/api/gen from ../api/dcctl.swagger.json
```

- The browser calls `/dcctl/api/...`: nginx serves dcctl under `/dcctl/` and strips
  the prefix. `dev:backend` does the same and proxies to `VITE_API_PROXY`, default `http://127.0.0.1:10022`.
- The mock accepts the user `support` with the password `mock-password`. Its
  responses are the examples of `api/dcctl.swagger.json`; the mock is not part
  of the production build.
- `src/api/gen` is generated and committed; do not edit it by hand.
