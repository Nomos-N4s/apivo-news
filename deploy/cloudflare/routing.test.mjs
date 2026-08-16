// The Worker's routing rules, proved with the standard web objects only.
//
// Run by `node --test deploy/cloudflare/` — no dependency, no bundler, no
// Cloudflare runtime. Everything a real deploy alone can prove (that a
// container boots, that the Durable Object wakes) stays out of here; what is
// in here is the decision-making that used to live only in a deployed
// Worker nobody could run.

import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { describe, it } from 'node:test';

import {
	CRAWLER_SIGNATURES,
	EDITORIAL_PREFIX,
	HSTS_VALUE,
	X_ROBOTS_TAG_VALUE,
	containerRequest,
	crawlerRefusal,
	isApiPath,
	isEditorialPath,
	matchesCrawlerSignature,
	rewriteSameSiteOriginHeaders,
	varyOnUserAgent,
	withEdgeHeaders,
} from './routing.js';

const SITE_HOST = 'news.example';

describe('isApiPath', () => {
	it('routes the API namespace and the probe endpoints to the api', () => {
		for (const path of [
			'/api',
			'/api/',
			'/api/v1/front',
			'/api/v1/editorial/queue',
			'/healthz',
			'/readyz',
		]) {
			assert.equal(isApiPath(path), true, path);
		}
	});

	it('leaves every reader and editorial page with the web container', () => {
		for (const path of [
			'/',
			'/el/munich',
			'/el/munich/a/123',
			'/de/editor',
			'/robots.txt',
			'/_astro/index.abc123.css',
			'/favicon.ico',
		]) {
			assert.equal(isApiPath(path), false, path);
		}
	});

	it('does not hand the api a path that merely starts with the letters', () => {
		// `/apis-of-the-city` is a place slug away from being a real reader
		// URL; only the segment boundary decides.
		assert.equal(isApiPath('/apis'), false);
		assert.equal(isApiPath('/apidocs'), false);
	});
});

describe('isEditorialPath', () => {
	it('covers exactly the prefix the Go binary mounts editorial under', () => {
		assert.equal(EDITORIAL_PREFIX, '/api/v1/editorial/');
		assert.equal(isEditorialPath('/api/v1/editorial/queue'), true);
		assert.equal(isEditorialPath('/api/v1/editorial/'), true);
	});

	it('does not rate-limit the reader endpoints', () => {
		assert.equal(isEditorialPath('/api/v1/front'), false);
		assert.equal(isEditorialPath('/api/v1/articles/123'), false);
		assert.equal(isEditorialPath('/el/editor'), false);
	});
});

describe('rewriteSameSiteOriginHeaders', () => {
	it("translates the site's own https origin to the http the container sees", () => {
		const headers = new Headers({ origin: `https://${SITE_HOST}` });
		rewriteSameSiteOriginHeaders(headers, SITE_HOST);
		assert.equal(headers.get('origin'), `http://${SITE_HOST}`);
	});

	it('leaves a foreign origin untouched, so the CSRF refusal still holds', () => {
		const headers = new Headers({ origin: 'https://evil.example' });
		rewriteSameSiteOriginHeaders(headers, SITE_HOST);
		assert.equal(headers.get('origin'), 'https://evil.example');
	});

	it('does not rewrite a look-alike host or a different port', () => {
		for (const origin of [
			`https://${SITE_HOST}.evil.example`,
			`https://evil.example.${SITE_HOST}`,
			`https://${SITE_HOST}:8443`,
			`http://${SITE_HOST}`,
		]) {
			const headers = new Headers({ origin });
			rewriteSameSiteOriginHeaders(headers, SITE_HOST);
			assert.equal(headers.get('origin'), origin, origin);
		}
	});

	it("translates the site's own Referer, path and query intact", () => {
		const headers = new Headers({
			referer: `https://${SITE_HOST}/el/editor?item=42`,
		});
		rewriteSameSiteOriginHeaders(headers, SITE_HOST);
		assert.equal(headers.get('referer'), `http://${SITE_HOST}/el/editor?item=42`);
	});

	it('leaves a foreign or unparseable Referer exactly as it arrived', () => {
		for (const referer of ['https://evil.example/x', 'not a url']) {
			const headers = new Headers({ referer });
			rewriteSameSiteOriginHeaders(headers, SITE_HOST);
			assert.equal(headers.get('referer'), referer, referer);
		}
	});

	it('adds nothing when the request carries neither header', () => {
		const headers = new Headers();
		rewriteSameSiteOriginHeaders(headers, SITE_HOST);
		assert.equal(headers.get('origin'), null);
		assert.equal(headers.get('referer'), null);
	});
});

describe('containerRequest', () => {
	it('proxies over http, keeping host, path and query', () => {
		const proxied = containerRequest(
			new Request(`https://${SITE_HOST}/api/v1/front?lang=el&place=munich`),
		);
		assert.equal(
			proxied.url,
			`http://${SITE_HOST}/api/v1/front?lang=el&place=munich`,
		);
	});

	it('carries the method, the body and the headers across', async () => {
		const proxied = containerRequest(
			new Request(`https://${SITE_HOST}/el/editor`, {
				method: 'POST',
				headers: {
					origin: `https://${SITE_HOST}`,
					referer: `https://${SITE_HOST}/el/editor`,
					'content-type': 'application/x-www-form-urlencoded',
				},
				body: 'action=approve&id=42',
			}),
		);
		assert.equal(proxied.method, 'POST');
		assert.equal(proxied.headers.get('content-type'), 'application/x-www-form-urlencoded');
		assert.equal(await proxied.text(), 'action=approve&id=42');
	});

	it('is what makes a same-site form post survive the proxy hop', () => {
		const proxied = containerRequest(
			new Request(`https://${SITE_HOST}/el/editor`, {
				method: 'POST',
				headers: { origin: `https://${SITE_HOST}` },
				body: 'action=approve',
			}),
		);
		// The container's own notion of its origin, which csrf.ts compares
		// against, is derived from exactly this URL.
		assert.equal(proxied.headers.get('origin'), new URL(proxied.url).origin);
	});

	it('states the real public scheme, which the rewrite above erased', () => {
		// The container is reached over plain HTTP and cannot see the TLS
		// hop in front of it. Without this header the web container writes
		// its session cookie without `Secure` on an https-only site.
		const proxied = containerRequest(new Request(`https://${SITE_HOST}/el/editor`));
		assert.equal(new URL(proxied.url).protocol, 'http:');
		assert.equal(proxied.headers.get('x-forwarded-proto'), 'https');
	});

	it("overwrites a client's own X-Forwarded-Proto rather than trusting it", () => {
		const proxied = containerRequest(
			new Request(`https://${SITE_HOST}/el/editor`, {
				headers: { 'x-forwarded-proto': 'http' },
			}),
		);
		assert.equal(proxied.headers.get('x-forwarded-proto'), 'https');
	});

	it('states http when the public hop really is http, as on wrangler dev', () => {
		const proxied = containerRequest(new Request(`http://localhost:8787/el/editor`));
		assert.equal(proxied.headers.get('x-forwarded-proto'), 'http');
	});

	it('never makes a cross-site post look same-site', () => {
		const proxied = containerRequest(
			new Request(`https://${SITE_HOST}/el/editor`, {
				method: 'POST',
				headers: { origin: 'https://evil.example' },
				body: 'action=approve',
			}),
		);
		assert.notEqual(proxied.headers.get('origin'), new URL(proxied.url).origin);
	});
});

describe('the crawler fence', () => {
	it('refuses a declared crawler with a 403, not advice', () => {
		assert.equal(matchesCrawlerSignature('Mozilla/5.0 (compatible; GPTBot/1.2)'), true);
		const refusal = crawlerRefusal();
		assert.equal(refusal.status, 403);
		assert.equal(refusal.headers.get('vary'), 'User-Agent');
	});

	it('matches case-insensitively, as a User-Agent substring', () => {
		assert.equal(matchesCrawlerSignature('claudebot/1.0'), true);
		assert.equal(matchesCrawlerSignature('x archive.org_bot y'), true);
	});

	it('does not catch an ordinary browser, or a client with no User-Agent', () => {
		assert.equal(
			matchesCrawlerSignature(
				'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 Safari/605.1.15',
			),
			false,
		);
		assert.equal(matchesCrawlerSignature(null), false);
		assert.equal(matchesCrawlerSignature(undefined), false);
	});

	// The list exists twice: here, and in the web container's middleware,
	// which is the copy Kubernetes relies on. Neither can be deleted, so
	// the only defence against drift is this - a bot added to one and not
	// the other fails the build with both lists printed.
	it('is identical to the web container middleware list', () => {
		const middleware = readFileSync(
			fileURLToPath(new URL('../../web/src/middleware.ts', import.meta.url)),
			'utf8',
		);
		const declaration = middleware.match(
			/CRAWLER_SIGNATURES:\s*readonly string\[\]\s*=\s*\[([\s\S]*?)\];/,
		);
		assert.ok(
			declaration,
			'could not find CRAWLER_SIGNATURES in web/src/middleware.ts - if it moved or was renamed, this drift check moved with it',
		);
		const fromMiddleware = [...declaration[1].matchAll(/'([^']+)'/g)].map(
			(match) => match[1],
		);
		assert.deepEqual(CRAWLER_SIGNATURES, fromMiddleware);
	});
});

describe('withEdgeHeaders', () => {
	it('stamps a response that carries no indexing advice of its own', async () => {
		const stamped = withEdgeHeaders(
			new Response('body', { status: 200, headers: { 'content-type': 'text/css' } }),
			true,
		);
		assert.equal(stamped.headers.get('x-robots-tag'), X_ROBOTS_TAG_VALUE);
		assert.equal(stamped.status, 200);
		assert.equal(stamped.headers.get('content-type'), 'text/css');
		assert.equal(await stamped.text(), 'body');
	});

	it("keeps the origin's own value: the Worker is a floor, not an override", () => {
		const stamped = withEdgeHeaders(
			new Response('body', {
				headers: {
					'x-robots-tag': 'noindex, noarchive',
					'strict-transport-security': 'max-age=60',
				},
			}),
			true,
		);
		assert.equal(stamped.headers.get('x-robots-tag'), 'noindex, noarchive');
		assert.equal(stamped.headers.get('strict-transport-security'), 'max-age=60');
	});

	it('handles a bodyless response', () => {
		const stamped = withEdgeHeaders(new Response(null, { status: 304 }), true);
		assert.equal(stamped.status, 304);
		assert.equal(stamped.headers.get('x-robots-tag'), X_ROBOTS_TAG_VALUE);
	});

	// workers.dev sits under an HSTS-preloaded TLD; a custom domain, which
	// RELEASING.md names as a supported target for this same deploy, has
	// nothing supplying the policy. The Worker is the only place that
	// knows the public scheme, so it is where the policy is stated.
	it('states the HSTS policy on an https request', () => {
		const stamped = withEdgeHeaders(new Response('body'), true);
		assert.equal(stamped.headers.get('strict-transport-security'), HSTS_VALUE);
		assert.equal(HSTS_VALUE, 'max-age=31536000; includeSubDomains');
	});

	// `preload` is a submission to a browser-vendor list and a commitment
	// that outlives the domain. A header may not make it on the founder's
	// behalf.
	it('does not claim preload', () => {
		assert.equal(HSTS_VALUE.includes('preload'), false);
	});

	it('states no policy over cleartext, where it would prove nothing', () => {
		const stamped = withEdgeHeaders(new Response('body'), false);
		assert.equal(stamped.headers.get('strict-transport-security'), null);
		assert.equal(stamped.headers.get('x-robots-tag'), X_ROBOTS_TAG_VALUE);
	});

	// The crawler fence answers 403 to some callers and passes the rest
	// through, so EVERY response varies on User-Agent - not only the
	// refusal. A shared cache that stored the allowed response could
	// otherwise hand it to a denied crawler, or hand a reader the denial.
	it('declares the User-Agent variance on an allowed response too', () => {
		assert.equal(withEdgeHeaders(new Response('body'), true).headers.get('vary'), 'User-Agent');
	});
});

describe('varyOnUserAgent', () => {
	it('adds the field to a response that declares no variance', () => {
		const headers = new Headers();
		varyOnUserAgent(headers);
		assert.equal(headers.get('vary'), 'User-Agent');
	});

	it("appends without displacing the origin's own fields", () => {
		const headers = new Headers({ vary: 'Accept-Encoding' });
		varyOnUserAgent(headers);
		assert.equal(headers.get('vary'), 'Accept-Encoding, User-Agent');
	});

	it('says it once, however the origin spelled it', () => {
		for (const vary of ['User-Agent', 'user-agent', 'Accept-Encoding, user-agent']) {
			const headers = new Headers({ vary });
			varyOnUserAgent(headers);
			assert.equal(headers.get('vary'), vary, vary);
		}
	});

	it('leaves `*` alone: appending to it could only make it less true', () => {
		const headers = new Headers({ vary: '*' });
		varyOnUserAgent(headers);
		assert.equal(headers.get('vary'), '*');
	});
});
