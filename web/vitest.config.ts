/// <reference types="vitest/config" />
import { getViteConfig } from 'astro/config';

// getViteConfig resolves Astro's virtual modules (astro:middleware) so the
// middleware can be imported and tested directly.
//
// Coverage scope: the gate measures application source that carries logic —
// today that is the middleware only. Generated code (src/lib/database.types.ts),
// ambient declarations (src/env.d.ts), and logic-free .astro pages are
// deliberately excluded: they would dilute the signal without measuring
// anything we author. New source files with logic must be added to `include`
// alongside their tests.
export default getViteConfig({
  test: {
    include: ['src/**/*.test.ts'],
    coverage: {
      provider: 'v8',
      include: ['src/middleware.ts'],
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
