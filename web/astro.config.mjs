// @ts-check
import { defineConfig } from 'astro/config';
import node from '@astrojs/node';

// The Node adapter keeps the frontend a plain container artefact, portable
// across Cloudflare Containers and Kubernetes alike. Nothing
// platform-specific belongs in this configuration.
export default defineConfig({
  output: 'server',
  adapter: node({ mode: 'standalone' }),
});
