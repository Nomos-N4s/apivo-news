/// <reference types="vitest/config" />
import { getViteConfig } from 'astro/config';

// getViteConfig resolves Astro's virtual modules (astro:middleware) so the
// middleware can be imported and tested directly.
//
// Coverage scope: the gate measures application source that carries logic —
// the middleware and the reader lib. Generated code (src/lib/database.types.ts),
// ambient declarations (src/env.d.ts), fixture data (src/lib/reader/fixtures.ts)
// and logic-free .astro pages are deliberately excluded: they would dilute the
// signal without measuring anything we author. New source files with logic must
// be added to `include` alongside their tests.
//
// src/lib/editorial/supabase.ts is excluded because it is the adapter over
// @supabase/ssr: covering it would mean asserting against a mocked SDK,
// which measures the mock. That exclusion is only honest because every
// decision of ours is out of the file and measured in session.ts — the
// config check, the claims mapping, and the cookie hardening
// (astroCookieOptions forcing httpOnly and deriving secure), which are
// security decisions and tested as such. A new branch in supabase.ts that
// is more than SDK plumbing must move to tested code the same way.
export default getViteConfig({
  test: {
    include: ['src/**/*.test.ts'],
    coverage: {
      provider: 'v8',
      include: [
        'src/middleware.ts',
        'src/lib/reader/api.ts',
        'src/lib/reader/axes.ts',
        'src/lib/reader/format.ts',
        'src/lib/reader/strings.ts',
        'src/lib/account/consent.ts',
        'src/lib/csrf.ts',
        'src/lib/editorial/api.ts',
        'src/lib/editorial/session.ts',
        'src/lib/editorial/strings.ts',
      ],
      // Constitution: TypeScript coverage minimum is 80%, enforced in CI —
      // the build fails below these thresholds.
      thresholds: {
        statements: 80,
        branches: 80,
        functions: 80,
        lines: 80,
      },
    },
  },
});
