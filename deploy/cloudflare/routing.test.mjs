// The Worker's routing rules, proved with the standard web objects only.
//
// Run by `node --test deploy/cloudflare/` — no dependency, no bundler, no
// Cloudflare runtime. Everything a real deploy alone can prove (that a
// container boots, that the Durable Object wakes) stays out of here; what is
// in here is the decision-making that used to live only in a deployed
// Worker nobody could run.

import assert from 'node:assert/strict';
import { describe, it } from 'node:test';

import {
	EDITORIAL_PREFIX,
	X_ROBOTS_TAG_VALUE,
	containerRequest,
	isApiPath,
	isEditorialPath,
	rewriteSameSiteOriginHeaders,
	withRobotsTag,
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

describe('withRobotsTag', () => {
	it('stamps a response that carries no indexing advice of its own', async () => {
		const stamped = withRobotsTag(
			new Response('body', { status: 200, headers: { 'content-type': 'text/css' } }),
		);
		assert.equal(stamped.headers.get('x-robots-tag'), X_ROBOTS_TAG_VALUE);
		assert.equal(stamped.status, 200);
		assert.equal(stamped.headers.get('content-type'), 'text/css');
		assert.equal(await stamped.text(), 'body');
	});

	it("keeps the origin's own value: the Worker is a floor, not an override", () => {
		const stamped = withRobotsTag(
			new Response('body', { headers: { 'x-robots-tag': 'noindex, noarchive' } }),
		);
		assert.equal(stamped.headers.get('x-robots-tag'), 'noindex, noarchive');
	});

	it('handles a bodyless response', () => {
		const stamped = withRobotsTag(new Response(null, { status: 304 }));
		assert.equal(stamped.status, 304);
		assert.equal(stamped.headers.get('x-robots-tag'), X_ROBOTS_TAG_VALUE);
	});
});
