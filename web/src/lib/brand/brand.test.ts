import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';

import {
  BRAND_FILE_NAME,
  BrandError,
  assertBrand,
  brandCustomProperties,
  brandStyleSheet,
  parseBrand,
  type Brand,
} from './index';

// The fixture brand the Go package is tested against, read from where
// the Go package keeps it. Not a copy: the same bytes, so a change to
// the file that only one of the two readers can cope with fails here.
//
// It is deliberately unlike the brand this repository ships — different
// name, palette, currency, default language and place (ADR-0004) — so a
// test that passes is evidence the code reads its configuration rather
// than remembering the product it was written for.
const fixtureUrl = new URL(
  `../../../../internal/platform/brand/testdata/fixture/${BRAND_FILE_NAME}`,
  import.meta.url,
);
const fixtureJson = readFileSync(fileURLToPath(fixtureUrl), 'utf8');

function fixture(): Brand {
  return parseBrand(fixtureJson);
}

/** The fixture brand as a plain object, for building invalid variants. */
function fixtureObject(): Record<string, unknown> {
  return JSON.parse(fixtureJson) as Record<string, unknown>;
}

describe('parseBrand', () => {
  it('reads the same fixture file the Go loader reads, to the same values', () => {
    expect(fixture()).toEqual({
      id: 'zephyra',
      name: 'Zephyra',
      legal: {
        entity: 'Zephyra Fixture Kooperativ AB',
        jurisdiction: 'SE',
        address: 'Kungsgatan 1, 411 19 Göteborg, Sweden',
        documents: {
          terms: { id: 'zephyra-terms', version: '3.1.0' },
          privacy: { id: 'zephyra-privacy', version: '2026-05-04' },
          cookies: { id: 'zephyra-cookies', version: '1.0.0' },
        },
      },
      domains: {
        primary: 'zephyra.example',
        aliases: ['www.zephyra.example', 'zephyra-forna.example'],
      },
      support: {
        general: 'hej@zephyra.example',
        legal: 'juridik@zephyra.example',
        privacy: 'dataskydd@www.zephyra.example',
      },
      assets: {
        logo: '/brand/zephyra/logo.svg',
        logoDark: '/brand/zephyra/logo-dark.svg',
        favicon: '/brand/zephyra/favicon.ico',
      },
      theme: {
        colours: {
          bg: '#0b1d26',
          surface: '#123243',
          text: '#eef6f8',
          accent: '#2ec4b6',
          'accent-2': '#ffbf47',
        },
        typography: {
          heading: '"Fraunces", Georgia, serif',
          body: '"Public Sans", system-ui, sans-serif',
          headingWeight: 700,
        },
      },
      defaults: { language: 'sv', place: 'goteborg', currency: 'SEK' },
      payout: { descriptor: 'ZEPHYRA CASHBACK' },
      features: {
        cashback: { wallet: true, withdrawal: true },
        news: { reader: false },
      },
    });
  });

  it('refuses anything that is not JSON', () => {
    expect(() => parseBrand('not json')).toThrow(BrandError);
    expect(() => parseBrand('not json')).toThrow(/not valid JSON/);
  });
});

describe('assertBrand', () => {
  it('accepts the fixture brand', () => {
    expect(() => {
      assertBrand(fixtureObject());
    }).not.toThrow();
  });

  const cases: ReadonlyArray<{
    readonly name: string;
    readonly mutate: (brand: Record<string, unknown>) => void;
    readonly problem: RegExp;
  }> = [
    {
      name: 'a missing field',
      mutate: (brand) => {
        delete brand['payout'];
      },
      problem: /payout is missing/,
    },
    {
      name: 'a missing nested field',
      mutate: (brand) => {
        delete (brand['legal'] as Record<string, unknown>)['entity'];
      },
      problem: /legal\.entity is missing/,
    },
    {
      name: 'a field of the wrong primitive type',
      mutate: (brand) => {
        brand['name'] = 42;
      },
      problem: /name is a number, want string/,
    },
    {
      name: 'a number where the schema wants one and a string arrived',
      mutate: (brand) => {
        (brand['theme'] as { typography: Record<string, unknown> }).typography['headingWeight'] = '700';
      },
      problem: /headingWeight is a string, want number/,
    },
    {
      name: 'a boolean flag that is not a boolean',
      mutate: (brand) => {
        (brand['features'] as { cashback: Record<string, unknown> }).cashback['wallet'] = 'yes';
      },
      problem: /features\.cashback\.wallet is a string, want boolean/,
    },
    {
      name: 'an object where a list belongs',
      mutate: (brand) => {
        (brand['domains'] as Record<string, unknown>)['aliases'] = { first: 'x' };
      },
      problem: /aliases is an object, want a list/,
    },
    {
      name: 'a list element of the wrong type',
      mutate: (brand) => {
        (brand['domains'] as Record<string, unknown>)['aliases'] = ['ok', 7];
      },
      problem: /aliases\[1\] is a number, want string/,
    },
    {
      name: 'a list where a map belongs',
      mutate: (brand) => {
        (brand['theme'] as Record<string, unknown>)['colours'] = ['#000000'];
      },
      problem: /colours is a list, want an object/,
    },
    {
      name: 'a nested interface that is not an object',
      mutate: (brand) => {
        brand['legal'] = 'Zephyra Fixture Kooperativ AB';
      },
      problem: /legal is a string, want an object/,
    },
    {
      name: 'a null where an interface belongs',
      mutate: (brand) => {
        brand['payout'] = null;
      },
      problem: /payout is null, want an object/,
    },
    {
      // A key nobody reads is a value that silently does not apply,
      // which is how a surface keeps the previous brand's colour.
      name: 'a key the schema has never heard of',
      mutate: (brand) => {
        brand['tagline'] = 'unknown to the schema';
      },
      problem: /tagline is not part of the brand schema/,
    },
    {
      name: 'a nested key the schema has never heard of',
      mutate: (brand) => {
        (brand['payout'] as Record<string, unknown>)['statementPrefix'] = 'ZEP';
      },
      problem: /payout\.statementPrefix is not part of the brand schema/,
    },
    {
      name: 'a map entry of the wrong shape',
      mutate: (brand) => {
        (brand['legal'] as { documents: Record<string, unknown> }).documents['terms'] = 'v3';
      },
      problem: /legal\.documents\.terms is a string, want an object/,
    },
  ];

  for (const testCase of cases) {
    it(`refuses ${testCase.name}`, () => {
      const brand = fixtureObject();
      testCase.mutate(brand);
      expect(() => {
        assertBrand(brand);
      }).toThrow(BrandError);
      expect(() => {
        assertBrand(brand);
      }).toThrow(testCase.problem);
    });
  }

  it('refuses a brand that is not an object at all', () => {
    expect(() => {
      assertBrand([]);
    }).toThrow(/the brand is a list, want an object/);
    expect(() => {
      assertBrand(null);
    }).toThrow(/the brand is null, want an object/);
  });

  it('names every problem at once, in a stable order', () => {
    const brand = fixtureObject();
    delete brand['payout'];
    brand['name'] = 42;
    let message = '';
    try {
      assertBrand(brand);
    } catch (error) {
      message = (error as BrandError).message;
    }
    expect(message).toMatch(/name is a number/);
    expect(message).toMatch(/payout is missing/);
    expect(message.indexOf('name is')).toBeLessThan(message.indexOf('payout is'));
  });

  // `in` walks the prototype chain. These two cases are what that costs,
  // and they are reachable because this module validates values built in
  // code, not only objects that came out of JSON.parse.
  it('does not read an inherited property as a field that is present', () => {
    const complete = fixtureObject();
    const payout = complete['payout'];
    delete complete['payout'];

    const inherited = Object.create({ payout }) as Record<string, unknown>;
    for (const [key, value] of Object.entries(complete)) {
      inherited[key] = value;
    }

    expect(() => {
      assertBrand(inherited);
    }).toThrow(/payout is missing/);
  });

  it('flags a key that only looks like a schema member because everything inherits it', () => {
    for (const key of ['toString', 'constructor', 'valueOf']) {
      const brand = fixtureObject();
      brand[key] = 'not part of the schema';
      expect(() => {
        assertBrand(brand);
      }).toThrow(new RegExp(`${key} is not part of the brand schema`));
    }
  });

  it('accepts a brand with no prototype at all', () => {
    const bare = Object.create(null) as Record<string, unknown>;
    for (const [key, value] of Object.entries(fixtureObject())) {
      bare[key] = value;
    }
    expect(() => {
      assertBrand(bare);
    }).not.toThrow();
  });

  // JSON carries no NaN, so this can only arrive from a value built in
  // code — which the loader is also used for.
  it('refuses a number that is not finite', () => {
    const brand = fixtureObject();
    (brand['theme'] as { typography: Record<string, unknown> }).typography['headingWeight'] = Number.NaN;
    expect(() => {
      assertBrand(brand);
    }).toThrow(/headingWeight is not a finite number/);
  });
});

describe('brandCustomProperties', () => {
  it('fills the variables the design system already declares', () => {
    expect(brandCustomProperties(fixture())).toEqual({
      '--color-accent': '#2ec4b6',
      '--color-accent-2': '#ffbf47',
      '--color-bg': '#0b1d26',
      '--color-surface': '#123243',
      '--color-text': '#eef6f8',
      '--font-heading': '"Fraunces", Georgia, serif',
      '--font-heading-weight': '700',
      '--font-body': '"Public Sans", system-ui, sans-serif',
    });
  });

  it('emits colour tokens in name order, so two builds of one brand match', () => {
    const names = Object.keys(brandCustomProperties(fixture())).filter((name) => name.startsWith('--color-'));
    expect(names).toEqual([...names].sort());
  });

  it('carries a brand token the design system has never seen', () => {
    const brand = fixtureObject();
    (brand['theme'] as { colours: Record<string, string> }).colours['highlight'] = '#ff00ff';
    assertBrand(brand);
    expect(brandCustomProperties(brand)['--color-highlight']).toBe('#ff00ff');
  });

  for (const [name, value] of [
    ['a declaration terminator', '#000000; background: url(x)'],
    ['a closing brace', '#000000 }'],
    ['a tag', '#000000</style'],
    ['a newline', '#000000\n'],
  ] as ReadonlyArray<readonly [string, string]>) {
    it(`refuses a value carrying ${name}`, () => {
      const brand = fixtureObject();
      (brand['theme'] as { colours: Record<string, string> }).colours['bg'] = value;
      assertBrand(brand);
      expect(() => brandCustomProperties(brand)).toThrow(BrandError);
      expect(() => brandCustomProperties(brand)).toThrow(/break out of the stylesheet/);
    });
  }

  // The schema promises Record<string, string>, and a map's KEYS carry
  // no type at all — so nothing upstream can vouch for a token name, and
  // the name is what goes into `--color-${token}`.
  for (const token of ['bg}', 'bg<style', 'Accent Two', '', '-leading-hyphen', 'bg;color:red']) {
    it(`refuses the colour token name ${JSON.stringify(token)}`, () => {
      const brand = fixtureObject();
      (brand['theme'] as { colours: Record<string, string> }).colours[token] = '#ffbf47';
      assertBrand(brand);
      expect(() => brandCustomProperties(brand)).toThrow(BrandError);
      expect(() => brandCustomProperties(brand)).toThrow(/is not a CSS token name/);
    });
  }

  it('refuses a font stack that would break out of the stylesheet', () => {
    const brand = fixtureObject();
    (brand['theme'] as { typography: Record<string, unknown> }).typography['body'] = 'serif; }';
    assertBrand(brand);
    expect(() => brandCustomProperties(brand)).toThrow(/theme\.typography\.body/);

    const heading = fixtureObject();
    (heading['theme'] as { typography: Record<string, unknown> }).typography['heading'] = 'serif; }';
    assertBrand(heading);
    expect(() => brandCustomProperties(heading)).toThrow(/theme\.typography\.heading/);
  });
});

describe('brandStyleSheet', () => {
  it('writes the custom properties as a rule', () => {
    expect(brandStyleSheet(fixture())).toBe(
      [
        ':root {',
        '  --color-accent: #2ec4b6;',
        '  --color-accent-2: #ffbf47;',
        '  --color-bg: #0b1d26;',
        '  --color-surface: #123243;',
        '  --color-text: #eef6f8;',
        '  --font-heading: "Fraunces", Georgia, serif;',
        '  --font-heading-weight: 700;',
        '  --font-body: "Public Sans", system-ui, sans-serif;',
        '}',
        '',
      ].join('\n'),
    );
  });

  it('accepts a selector other than the document root', () => {
    expect(brandStyleSheet(fixture(), '.brand-preview')).toMatch(/^\.brand-preview \{\n/);
    expect(brandStyleSheet(fixture(), '[data-brand="zephyra"] .card')).toMatch(/^\[data-brand="zephyra"\] \.card \{\n/);
  });

  // Every caller today passes a literal, but this is exported as a
  // general utility and its argument is an input like any other.
  for (const selector of [':root} body { display: none', ':root</style', ':root;', ':root\n}', '', '   ']) {
    it(`refuses the selector ${JSON.stringify(selector)}`, () => {
      expect(() => brandStyleSheet(fixture(), selector)).toThrow(BrandError);
      expect(() => brandStyleSheet(fixture(), selector)).toThrow(/is not a usable stylesheet selector/);
    });
  }
});
