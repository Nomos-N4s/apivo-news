/// <reference types="vitest/config" />
import { getViteConfig } from 'astro/config';

// getViteConfig resolves Astro's virtual modules (astro:middleware) so the
// middleware can be imported and tested directly. Coverage thresholds are
// deliberately not set here — the CI gate is a separate concern (T008).
export default getViteConfig({
  test: {
    include: ['src/**/*.test.ts'],
    coverage: {
      provider: 'v8',
      include: ['src/middleware.ts'],
    },
  },
});
