import { describe, expect, it } from 'vitest';

import { isSameOrigin } from './csrf';

const SITE = 'https://epiloyes.example';

function post(headers: Record<string, string>): Request {
  return new Request(`${SITE}/el/editor`, { method: 'POST', headers });
}

describe('isSameOrigin', () => {
  it('accepts a form post from this site', () => {
    expect(isSameOrigin(post({ origin: SITE }), SITE)).toBe(true);
  });

  it('refuses a post from another origin — the CSRF case', () => {
    expect(isSameOrigin(post({ origin: 'https://evil.example' }), SITE)).toBe(false);
  });

  it('falls back to Referer when Origin is absent', () => {
    expect(isSameOrigin(post({ referer: `${SITE}/el/editor?item=1` }), SITE)).toBe(true);
    expect(isSameOrigin(post({ referer: 'https://evil.example/x' }), SITE)).toBe(false);
  });

  it('refuses a post carrying neither header', () => {
    expect(isSameOrigin(post({}), SITE)).toBe(false);
  });

  it('refuses an unparseable Referer rather than trusting it', () => {
    expect(isSameOrigin(post({ referer: 'not a url' }), SITE)).toBe(false);
  });

  it('distinguishes scheme and port, not just host', () => {
    expect(isSameOrigin(post({ origin: 'http://epiloyes.example' }), SITE)).toBe(false);
    expect(isSameOrigin(post({ origin: `${SITE}:8443` }), SITE)).toBe(false);
  });
});
