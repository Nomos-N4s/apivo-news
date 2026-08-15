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
