import { describe, expect, it } from 'vitest';

import {
  ACCESS_OUTCOMES,
  DEFAULT_RETURN_PATH,
  confirmUrl,
  isAccessOutcome,
  safeReturnPath,
  signInPath,
} from './access';

describe('safeReturnPath', () => {
  it.each([
    '/',
    '/el/munich',
    '/el/munich/cashback/wallet',
    '/de/munich+greece/cashback/withdraw?state=pending',
    '/el/munich/cashback/agora#rates',
  ])('keeps %s, which is a path on this site', (path) => {
    expect(safeReturnPath(path)).toBe(path);
  });

  /*
   * Every one of these begins with a slash, and every one of them leaves
   * this site or goes nowhere useful. This list is the whole reason the
   * check is on the string rather than on `new URL(raw, origin)`, which
   * resolves the first two to another origin and calls them valid.
   */
  it.each([
    ['//evil.example/', 'protocol-relative'],
    ['//evil.example', 'protocol-relative, no trailing slash'],
    ['/\\evil.example', 'a backslash a browser normalises to a slash'],
    ['/el/munich\\@evil.example', 'a backslash anywhere'],
    ['https://evil.example/', 'an absolute URL'],
    ['el/munich', 'relative, so it depends on where it is read'],
    ['', 'empty'],
    ['/auth/confirm', 'back into the flow that just finished'],
    ['/auth', 'the same, without the segment'],
    ['/el/\u0000munich', 'a control character, which is how a URL gets split'],
    ['/el/munich\nLocation: https://evil.example', 'a newline, for the same reason'],
  ])('refuses %s (%s) and falls back', (raw) => {
    expect(safeReturnPath(raw)).toBe(DEFAULT_RETURN_PATH);
  });

  it('refuses a path long enough to be a payload rather than a path', () => {
    expect(safeReturnPath(`/${'a'.repeat(600)}`)).toBe(DEFAULT_RETURN_PATH);
  });

  it.each([null, undefined])('treats %s as absent', (raw) => {
    expect(safeReturnPath(raw)).toBe(DEFAULT_RETURN_PATH);
  });

  it('falls back to what the caller asked for, not always to the front page', () => {
    expect(safeReturnPath('//evil.example', '/el/munich')).toBe('/el/munich');
    expect(safeReturnPath(null, '')).toBe('');
  });
});

describe('isAccessOutcome', () => {
  it.each(ACCESS_OUTCOMES)('recognises %s', (outcome) => {
    expect(isAccessOutcome(outcome)).toBe(true);
  });

  it.each(['', 'SENT', 'signed-in', 'undefined', null, undefined])(
    'refuses %s, so the page renders no state it has no sentence for',
    (value) => {
      expect(isAccessOutcome(value)).toBe(false);
    },
  );
});

describe('signInPath', () => {
  it('is the bare page when there is nowhere to come back to', () => {
    expect(signInPath('el')).toBe('/el/signin');
    expect(signInPath('de')).toBe('/de/signin');
  });

  it('carries the return path', () => {
    expect(signInPath('el', { next: '/el/munich/cashback/wallet' })).toBe(
      '/el/signin?next=%2Fel%2Fmunich%2Fcashback%2Fwallet',
    );
  });

  it('carries an outcome', () => {
    expect(signInPath('de', { outcome: 'expired' })).toBe('/de/signin?outcome=expired');
  });

  it('drops a return path it would not honour, rather than passing it on', () => {
    expect(signInPath('el', { next: '//evil.example' })).toBe('/el/signin');
  });
});

describe('confirmUrl', () => {
  it('is absolute, because the provider redirects from its own origin', () => {
    expect(confirmUrl('https://ra1ze.com', 'el', '/el/munich/cashback')).toBe(
      'https://ra1ze.com/auth/confirm?lang=el&next=%2Fel%2Fmunich%2Fcashback',
    );
  });

  it('carries the language, which the route has none of its own', () => {
    expect(confirmUrl('https://ra1ze.com', 'de', '')).toBe(
      'https://ra1ze.com/auth/confirm?lang=de',
    );
  });

  it('refuses to carry a return path it would not honour', () => {
    expect(confirmUrl('https://ra1ze.com', 'el', 'https://evil.example')).toBe(
      'https://ra1ze.com/auth/confirm?lang=el',
    );
  });
});
