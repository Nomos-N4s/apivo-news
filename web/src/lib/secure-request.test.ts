import { describe, expect, it } from 'vitest';

import { forwardedProto, isSecureRequest } from './secure-request';

const httpRequest = (headers: Record<string, string> = {}): Request =>
  new Request('http://news.example/el/munich', { headers });

describe('isSecureRequest', () => {
  // The finding this module exists for: the Worker proxies to the
  // container over plain HTTP, so the request URL says `http:` on an
  // https-only deployment. A cookie written from that URL loses `Secure`
  // and a refresh token rides the next cleartext request.
  it('is true in a deployed environment even though the request URL is http', () => {
    const request = httpRequest();
    expect(new URL(request.url).protocol).toBe('http:');
    expect(isSecureRequest(request, 'prod')).toBe(true);
  });

  it('is true in prod whatever a forwarding hop claims', () => {
    // No proxy header may lower the deployed answer: the deployment is
    // https-only, and the only thing a downgrade could achieve is a
    // session cookie on the wire.
    expect(isSecureRequest(httpRequest({ 'x-forwarded-proto': 'http' }), 'prod')).toBe(true);
  });

  it('reads the forwarded scheme outside prod, where a TLS ingress states it', () => {
    expect(isSecureRequest(httpRequest({ 'x-forwarded-proto': 'https' }), 'dev')).toBe(true);
    expect(isSecureRequest(httpRequest({ 'x-forwarded-proto': 'http' }), 'dev')).toBe(false);
    expect(isSecureRequest(httpRequest({ 'x-forwarded-proto': 'HTTPS' }), undefined)).toBe(true);
  });

  it('takes the hop closest to the browser when several appended', () => {
    expect(isSecureRequest(httpRequest({ 'x-forwarded-proto': 'https, http' }), 'dev')).toBe(true);
    expect(isSecureRequest(httpRequest({ 'x-forwarded-proto': 'http, https' }), 'dev')).toBe(false);
  });

  it('falls back to the request URL on the development stack', () => {
    expect(isSecureRequest(httpRequest(), 'dev')).toBe(false);
    expect(isSecureRequest(httpRequest(), undefined)).toBe(false);
    expect(isSecureRequest(new Request('https://news.example/'), 'dev')).toBe(true);
  });

  // An APP_ENV nobody meant is not prod here — but it is refused at the
  // reader boundary (lib/reader/api.ts) before a page renders, and the
  // Worker's forwarded scheme still answers true behind it.
  it('does not read an unknown APP_ENV as deployed', () => {
    expect(isSecureRequest(httpRequest(), 'production')).toBe(false);
    expect(isSecureRequest(httpRequest({ 'x-forwarded-proto': 'https' }), 'production')).toBe(true);
  });
});

describe('forwardedProto', () => {
  it('reports no declaration as null, including an empty one', () => {
    expect(forwardedProto(null)).toBe(null);
    expect(forwardedProto(undefined)).toBe(null);
    expect(forwardedProto('')).toBe(null);
    expect(forwardedProto('  ')).toBe(null);
  });

  it('lower-cases and trims what it reports', () => {
    expect(forwardedProto(' HTTPS ')).toBe('https');
  });
});
