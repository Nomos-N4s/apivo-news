import { describe, expect, it } from 'vitest';

import {
  articlePath,
  DEFAULT_FRONT_PAGE,
  findPlace,
  frontPagePath,
  isReadingLanguage,
  LANGUAGE_ENDONYMS,
  parseAxes,
  PLACE_CATALOG,
  placeKicker,
} from './axes';

describe('isReadingLanguage', () => {
  it('accepts exactly the alpha languages', () => {
    expect(isReadingLanguage('el')).toBe(true);
    expect(isReadingLanguage('de')).toBe(true);
  });

  it('rejects everything else — en exists in the schema and is not reachable (FR-015)', () => {
    expect(isReadingLanguage('en')).toBe(false);
    expect(isReadingLanguage('el-DE')).toBe(false);
    expect(isReadingLanguage('')).toBe(false);
    expect(isReadingLanguage('EL')).toBe(false);
  });
});

describe('parseAxes', () => {
  it('parses a language and a single place', () => {
    const axes = parseAxes('el', 'munich');
    expect(axes).not.toBeNull();
    expect(axes?.lang).toBe('el');
    expect(axes?.places.map((place) => place.slug)).toEqual(['munich']);
  });

  it('parses multiple places joined by + in URL order', () => {
    const axes = parseAxes('de', 'munich+greece');
    expect(axes?.lang).toBe('de');
    expect(axes?.places.map((place) => place.slug)).toEqual(['munich', 'greece']);
  });

  it('collapses duplicate slugs to their first occurrence', () => {
    const axes = parseAxes('el', 'greece+munich+greece');
    expect(axes?.places.map((place) => place.slug)).toEqual(['greece', 'munich']);
  });

  it('accepts non-selectable catalog places — they are addressable, just not offered', () => {
    const axes = parseAxes('el', 'bavaria');
    expect(axes?.places.at(0)?.selectable).toBe(false);
  });

  it('rejects an unmounted language', () => {
    expect(parseAxes('en', 'munich')).toBeNull();
    expect(parseAxes(undefined, 'munich')).toBeNull();
  });

  it('rejects unknown or empty place segments', () => {
    expect(parseAxes('el', 'atlantis')).toBeNull();
    expect(parseAxes('el', 'munich+atlantis')).toBeNull();
    expect(parseAxes('el', '')).toBeNull();
    expect(parseAxes('el', undefined)).toBeNull();
    expect(parseAxes('el', '+')).toBeNull();
  });
});

describe('frontPagePath', () => {
  it('composes /{lang}/{place[+place]}', () => {
    expect(frontPagePath('el', ['munich'])).toBe('/el/munich');
    expect(frontPagePath('de', ['munich', 'greece'])).toBe('/de/munich+greece');
  });

  it('refuses an empty place list — a front page always has both axes', () => {
    expect(() => frontPagePath('el', [])).toThrow();
  });

  it('lands the flagship journey on /el/munich+greece', () => {
    expect(DEFAULT_FRONT_PAGE).toBe('/el/munich+greece');
  });
});

describe('articlePath', () => {
  it('keeps both axes in the article URL (FR-009)', () => {
    expect(articlePath('el', ['munich', 'greece'], 'abc-123')).toBe(
      '/el/munich+greece/a/abc-123',
    );
  });

  it('escapes ids that need it', () => {
    expect(articlePath('de', ['munich'], 'a/b')).toBe('/de/munich/a/a%2Fb');
  });
});

describe('placeKicker', () => {
  const labels = { city: 'Local', region: 'Region', country: 'National' } as const;

  it('joins the first place endonym with its scope label', () => {
    expect(placeKicker(['munich'], labels)).toBe('München · Local');
    expect(placeKicker(['greece', 'munich'], labels)).toBe('Ελλάδα · National');
  });

  it('renders unknown slugs as themselves and empty lists as nothing', () => {
    expect(placeKicker(['atlantis'], labels)).toBe('atlantis');
    expect(placeKicker([], labels)).toBe('');
  });
});

describe('LANGUAGE_ENDONYMS', () => {
  it('names each language in itself', () => {
    expect(LANGUAGE_ENDONYMS.el).toBe('Ελληνικά');
    expect(LANGUAGE_ENDONYMS.de).toBe('Deutsch');
  });
});

describe('the place catalog', () => {
  it('mirrors the 0002 seeds', () => {
    expect(PLACE_CATALOG.map((place) => place.slug).sort()).toEqual([
      'bavaria',
      'germany',
      'greece',
      'munich',
    ]);
  });

  it('finds places by slug and nothing else', () => {
    expect(findPlace('munich')?.endonym).toBe('München');
    expect(findPlace('greece')?.scope).toBe('country');
    expect(findPlace('münchen')).toBeUndefined();
  });
});
