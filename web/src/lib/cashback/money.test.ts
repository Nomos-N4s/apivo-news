import { describe, expect, it } from 'vitest';

import {
  currencyDigits,
  decimalString,
  formatMoney,
  formatRateBps,
  formatSignedMoney,
  isMoney,
  parseAmountToMinor,
  sum,
  zero,
} from './money';

/**
 * ICU separates a number from its unit with U+00A0 (and U+202F in some
 * locales). Asserting on the exact code point would make these tests a
 * record of the CLDR version the runner happens to carry, so the assertions
 * that compare whole strings compare them with every kind of space
 * normalised to one.
 */
const spaces = (value: string): string => value.replace(/[\u00a0\u202f\u2009]/g, ' ');

describe('decimalString', () => {
  it.each([
    [145, 2, '1.45'],
    [-145, 2, '-1.45'],
    [5, 2, '0.05'],
    [0, 2, '0.00'],
    [24060, 2, '240.60'],
    [1500, 0, '1500'],
    [-7, 3, '-0.007'],
  ])('renders %i minor units at %i digits as %s', (minor, digits, expected) => {
    expect(decimalString(minor, digits)).toBe(expected);
  });

  it('never loses a cent to floating point across a wide range', () => {
    // The whole reason this function exists rather than a division by 100.
    for (let minor = 0; minor < 2000; minor += 1) {
      const rendered = decimalString(minor, 2);
      expect(Number(rendered.replace('.', ''))).toBe(minor);
    }
  });

  it('is exact at a magnitude where a division by a hundred is not', () => {
    // 1_000_000_000_000_001 / 100 is 10000000000000.01 in decimal and
    // 10000000000000.012 in binary floating point.
    expect(decimalString(1_000_000_000_000_001, 2)).toBe('10000000000000.01');
  });
});

describe('formatMoney', () => {
  it('formats a credit in the reading language', () => {
    expect(formatMoney('de', { minor: 145, currency: 'EUR' })).toContain('1,45');
    expect(formatMoney('el', { minor: 24060, currency: 'EUR' })).toContain('240,60');
  });

  it('carries the currency symbol, never a bare number', () => {
    expect(formatMoney('de', { minor: 100, currency: 'EUR' })).toContain('€');
  });

  it('renders a reversal as negative', () => {
    expect(formatMoney('de', { minor: -422, currency: 'EUR' })).toMatch(/-|−/);
  });

  it('respects a currency written with no minor units', () => {
    expect(formatMoney('de', { minor: 1500, currency: 'JPY' })).toContain('1.500');
  });
});

describe('formatSignedMoney', () => {
  it('marks a credit with a plus so an entry list reads at a glance', () => {
    expect(formatSignedMoney('de', { minor: 378, currency: 'EUR' })).toMatch(/^\+/);
  });

  it('leaves a reversal to its own sign', () => {
    expect(formatSignedMoney('de', { minor: -378, currency: 'EUR' })).not.toMatch(/^\+/);
  });

  it('gives zero no sign — a declined entry paid nothing, it did not lose anything', () => {
    expect(formatSignedMoney('de', { minor: 0, currency: 'EUR' })).not.toMatch(/^\+/);
  });
});

describe('formatRateBps', () => {
  it('renders whole percentages without trailing zeroes', () => {
    expect(spaces(formatRateBps('de', 700))).toBe('7 %');
  });

  it('keeps a fractional band', () => {
    expect(spaces(formatRateBps('de', 725))).toBe('7,25 %');
  });

  it('renders a sub-percent band rather than rounding it to nothing', () => {
    expect(spaces(formatRateBps('de', 50))).toBe('0,5 %');
  });
});

describe('isMoney', () => {
  it.each([
    [{ minor: 100, currency: 'EUR' }, true],
    [{ minor: 1.5, currency: 'EUR' }, false],
    [{ minor: 100, currency: 'eur' }, false],
    [{ minor: 100, currency: 'EURO' }, false],
    [{ minor: '100', currency: 'EUR' }, false],
    [{ currency: 'EUR' }, false],
    [null, false],
    ['100 EUR', false],
  ])('judges %o as %s', (value, expected) => {
    expect(isMoney(value)).toBe(expected);
  });
});

describe('sum', () => {
  it('adds amounts in one currency', () => {
    expect(
      sum([
        { minor: 378, currency: 'EUR' },
        { minor: 145, currency: 'EUR' },
      ]),
    ).toEqual({ minor: 523, currency: 'EUR' });
  });

  it('refuses a mixed-currency total rather than picking a currency', () => {
    expect(() =>
      sum([
        { minor: 100, currency: 'EUR' },
        { minor: 100, currency: 'USD' },
      ]),
    ).toThrow(/spanning currencies/);
  });

  it('refuses to invent a currency for an empty sum', () => {
    expect(() => sum([])).toThrow(/no currency/);
  });
});

describe('zero', () => {
  it('keeps the currency an empty wallet is denominated in', () => {
    expect(zero('EUR')).toEqual({ minor: 0, currency: 'EUR' });
  });
});

describe('parseAmountToMinor', () => {
  it.each([
    ['19,99', 2, 1999],
    ['19.99', 2, 1999],
    ['20', 2, 2000],
    ['1.234,56', 2, 123456],
    ['1,234.56', 2, 123456],
    ['0,05', 2, 5],
    ['€ 240,60', 2, 24060],
    ['240,60 €', 2, 24060],
    ['1 500', 0, 1500],
  ])('reads %s at %i digits as %i minor units', (input, digits, expected) => {
    expect(parseAmountToMinor(input, digits)).toBe(expected);
  });

  it('reads a lone separator with the wrong number of decimals as grouping', () => {
    // "1.234" is a thousand two hundred and thirty-four euros, not 1.23.
    expect(parseAmountToMinor('1.234', 2)).toBe(123400);
    expect(parseAmountToMinor('1,234,567', 2)).toBe(123456700);
  });

  it.each(['', '   ', 'abc', '-5,00', '1,2,3.4.5', '..', ','])(
    'refuses %o rather than reading it as zero',
    (input) => {
      expect(parseAmountToMinor(input, 2)).toBeNull();
    },
  );

  it('refuses an amount too large to hold exactly', () => {
    expect(parseAmountToMinor('999999999999999999999', 2)).toBeNull();
  });

  it('round-trips against the formatter', () => {
    for (const minor of [1, 5, 99, 100, 1999, 24060, 100000]) {
      const rendered = decimalString(minor, 2);
      expect(parseAmountToMinor(rendered, 2)).toBe(minor);
    }
  });
});

describe('currencyDigits', () => {
  it('knows the euro has two and the yen none', () => {
    expect(currencyDigits('de', 'EUR')).toBe(2);
    expect(currencyDigits('de', 'JPY')).toBe(0);
  });
});
