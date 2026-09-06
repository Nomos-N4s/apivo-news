import { describe, expect, it } from 'vitest';

import { DEFAULT_OPS_LANGUAGE, opsLanguage } from './ops-language';
import { READING_LANGUAGES } from '../reader/axes';

const at = (search: string): URL => new URL(`http://localhost/ops/held${search}`);

describe('opsLanguage', () => {
  it('takes the language from the query', () => {
    expect(opsLanguage(at('?lang=de'))).toBe('de');
    expect(opsLanguage(at('?lang=el'))).toBe('el');
  });

  it('falls back to the default rather than to a tag nothing is keyed by', () => {
    expect(opsLanguage(at(''))).toBe(DEFAULT_OPS_LANGUAGE);
    expect(opsLanguage(at('?lang=fr'))).toBe(DEFAULT_OPS_LANGUAGE);
    expect(opsLanguage(at('?lang='))).toBe(DEFAULT_OPS_LANGUAGE);
    expect(opsLanguage(at('?lang=el-DE'))).toBe(DEFAULT_OPS_LANGUAGE);
  });

  it('defaults to a language the catalogues actually carry', () => {
    expect(READING_LANGUAGES).toContain(DEFAULT_OPS_LANGUAGE);
  });
});
