// @ts-check
import { defineConfig, envField } from 'astro/config';
import node from '@astrojs/node';

// The Node adapter keeps the frontend a plain container artefact, portable
// across Cloudflare Containers and Kubernetes alike. Nothing
// platform-specific belongs in this configuration.
export default defineConfig({
  output: 'server',
  adapter: node({ mode: 'standalone' }),
  env: {
    schema: {
      // The Go reader API on the internal network (compose/K8s service
      // address). Read at request time, so the same image serves every
      // environment. Unset, the reader pages answer from built-in
      // fixtures — the development preview until T023/T024 land.
      API_BASE_URL: envField.string({
        context: 'server',
        access: 'secret',
        optional: true,
      }),
    },
  },
});
