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

  // The PORT is part of the host and still distinguishes an origin: a
  // different port is a different site as far as the browser is concerned.
  it('distinguishes port', () => {
    expect(isSameOrigin(post({ origin: `${SITE}:8443` }), SITE)).toBe(false);
  });

  // The SCHEME deliberately does not, and this used to assert the opposite.
  //
  // @astrojs/node builds Astro.url from the socket and the Host header,
  // ignoring X-Forwarded-Proto, so behind TLS termination the site origin is
  // http://<host> while the browser sends https://<host>. Three proxies
  // compensated by rewriting the header, and each needs the hostname at
  // parse time — which a WILDCARD site does not have. So previews had no
  // rewrite and every editorial form post there, sign-in included, was
  // refused. That was written off as costing nothing "because a preview has
  // no auth to sign in with", which stopped being true when previews got
  // auth.
  //
  // CSRF is about which SITE posted, and the host is that answer. What is
  // given up is refusing a post from this same host over http, which needs
  // the site served over http at all — settled by isSecureRequest and the
  // redirect at the edge, not by a string comparison here.
  it('does not distinguish scheme, so the check survives any proxy', () => {
    expect(isSameOrigin(post({ origin: 'http://epiloyes.example' }), SITE)).toBe(true);
    expect(isSameOrigin(post({ origin: SITE }), 'http://epiloyes.example')).toBe(true);
  });

  it('still refuses another host over the same scheme', () => {
    expect(isSameOrigin(post({ origin: 'https://evil.example' }), SITE)).toBe(false);
    expect(isSameOrigin(post({ origin: 'http://evil.example' }), SITE)).toBe(false);
  });

  it('refuses an origin that is not a URL', () => {
    expect(isSameOrigin(post({ origin: 'null' }), SITE)).toBe(false);
    expect(isSameOrigin(post({ origin: 'not a url' }), SITE)).toBe(false);
  });
});
