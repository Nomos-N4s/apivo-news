import { describe, expect, it } from 'vitest';

import {
  DEFAULT_FRONT_PAGE,
  findPlace,
  frontPagePath,
  isReadingLanguage,
  parseAxes,
  PLACE_CATALOG,
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
