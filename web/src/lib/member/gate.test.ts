import { describe, expect, it } from 'vitest';

import { CashbackApiError } from '../cashback/api';
import { cashbackPageLanguage, isUnauthorized, needsSignIn } from './gate';

describe('needsSignIn', () => {
  it('stops a signed-out visitor where there is a real api', () => {
    expect(needsSignIn(false, false)).toBe(true);
  });

  it('lets a signed-in member through', () => {
    expect(needsSignIn(true, false)).toBe(false);
  });

  it.each([true, false])(
    'never stops anybody in fixture mode, signed in %s',
    (authenticated) => {
      // A pull-request preview runs with no api and nobody signed in, and its
      // whole job is to render the screens for review. Fencing it would make
      // previews useless for the one thing they exist for.
      expect(needsSignIn(authenticated, true)).toBe(false);
    },
  );
});

describe('cashbackPageLanguage', () => {
  it.each([
    ['/el/munich/cashback', 'el'],
    ['/el/munich/cashback/', 'el'],
    ['/de/munich/cashback/wallet', 'de'],
    ['/de/munich+greece/cashback/withdraw', 'de'],
    ['/el/munich/cashback/agora', 'el'],
  ])('reads %s as a member page in %s', (path, lang) => {
    expect(cashbackPageLanguage(path)).toBe(lang);
  });

  it.each([
    ['/ops', 'the operator queues have a role to check and no language'],
    ['/ops/held', 'the same'],
    ['/api/cashback/clickout', 'a form posts here; a redirect would lose it'],
    ['/', 'the front page'],
    ['/el/munich', 'the news'],
    ['/el/signin', 'the sign-in page itself'],
    ['/el/munich/cashbackery', 'a prefix that only looks like one'],
    ['/el/cashback', 'no place segment, so not a member page'],
  ])('leaves %s alone (%s)', (path) => {
    expect(cashbackPageLanguage(path)).toBeNull();
  });

  it.each(['/fr/munich/cashback', '/en/munich/cashback/wallet'])(
    'answers null for %s rather than guessing a language',
    (path) => {
      // The page's own guard rewrites these to /404. A fence that redirected
      // them to a Greek sign-in would be inventing a reader's language on the
      // way past (FR-009, FR-015).
      expect(cashbackPageLanguage(path)).toBeNull();
    },
  );
});

describe('isUnauthorized', () => {
  it.each([401, 403])('recognises %d as the api declining the caller', (status) => {
    expect(isUnauthorized(new CashbackApiError('refused', status, null))).toBe(true);
  });

  it.each([404, 409, 429, 500, 502, 503])(
    'leaves %d alone, because it is not about who is asking',
    (status) => {
      expect(isUnauthorized(new CashbackApiError('answered', status, null))).toBe(false);
    },
  );

  it.each([
    ['a plain error', new Error('boom')],
    ['a string', 'boom'],
    ['null', null],
    ['undefined', undefined],
  ])('is false for %s, which carries no status at all', (_label, thrown) => {
    expect(isUnauthorized(thrown)).toBe(false);
  });
});
