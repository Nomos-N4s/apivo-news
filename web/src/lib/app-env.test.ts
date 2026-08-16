import { describe, expect, it } from 'vitest';

import { isProdEnv, parseAppEnv } from './app-env';

describe('parseAppEnv', () => {
  it('reads the two values the Go binary accepts', () => {
    expect(parseAppEnv('dev')).toBe('dev');
    expect(parseAppEnv('prod')).toBe('prod');
  });

  it('reads unset and empty as development, exactly as the binary does', () => {
    expect(parseAppEnv(undefined)).toBe('dev');
    expect(parseAppEnv(null)).toBe('dev');
    expect(parseAppEnv('')).toBe('dev');
  });

  // The value most likely to be typed by hand is `production`, and reading
  // it as "not prod" would serve a deployed reader from fixtures whose
  // publishers and approving editor are invented.
  it('reports a value it does not understand as unknown, never as development', () => {
    for (const raw of ['production', 'PROD', 'staging', 'prod ']) {
      expect(parseAppEnv(raw), raw).toBe(null);
    }
  });
});

describe('isProdEnv', () => {
  it('is true only for the exact deployed value', () => {
    expect(isProdEnv('prod')).toBe(true);
    expect(isProdEnv('dev')).toBe(false);
    expect(isProdEnv(undefined)).toBe(false);
    expect(isProdEnv('production')).toBe(false);
  });
});
