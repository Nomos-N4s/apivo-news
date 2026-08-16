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
      // Supabase Auth, for the editorial sign-in. Both keep the PUBLIC_
      // names the Supabase tooling uses — the anon key is public by
      // design, it identifies the project and grants nothing on its own.
      //
      // They are nevertheless declared `access: 'secret'`, which in Astro
      // means "read from the environment at runtime" rather than "inlined
      // at build". Same reason as API_BASE_URL above: one image, many
      // environments. A build-inlined value would bake one project into
      // the container.
      //
      // Optional, and unset is a working state: the sign-in page says the
      // deployment has no auth configured and the editorial screens keep
      // their fixture preview, exactly as the api does with JWKS_URL.
      PUBLIC_SUPABASE_URL: envField.string({
        context: 'server',
        access: 'secret',
        optional: true,
      }),
      PUBLIC_SUPABASE_ANON_KEY: envField.string({
        context: 'server',
        access: 'secret',
        optional: true,
      }),
      // The release version (issue #119): the annotated tag the release
      // pipeline deployed, rendered in the footer's fine print. PUBLIC_ by
      // nature — it is printed on every page — but declared
      // `access: 'secret'` for the same reason as everything above: read
      // from the environment at runtime, so the version names the deploy,
      // not whichever build produced the image. Unset is a working state
      // and renders nothing: only the release pipeline may claim a
      // version, and an unstamped deployment must not invent one.
      PUBLIC_APP_VERSION: envField.string({
        context: 'server',
        access: 'secret',
        optional: true,
      }),
    },
  },
});
