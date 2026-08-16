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
	isRateLimitedApiPath,
	isReaderApiPath,
	bearerToken,
	matchesCrawlerSignature,
	normalisePath,
	RATE_LIMIT_KEY_MAX,
	rateLimitKey,
	rewriteSameSiteOriginHeaders,
	tokenSubject,
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

describe('normalisePath', () => {
	// The bypass this function exists for: the Worker saw the raw
	// percent-encoded pathname while Go's ServeMux matched after
	// unescaping, so one encoded character was not editorial here and was
	// editorial there - the limiter skipped, the handler ran.
	it('decodes an encoded ordinary character, as the Go router does', () => {
		assert.equal(normalisePath('/api/v1/%65ditorial/queue'), '/api/v1/editorial/queue');
		assert.equal(normalisePath('/%61pi/v1/front'), '/api/v1/front');
	});

	it('decodes once, not repeatedly: %2565 is not an `e` to Go either', () => {
		assert.equal(normalisePath('/api/v1/%2565ditorial/queue'), '/api/v1/%65ditorial/queue');
	});

	it('collapses repeated slashes, as path.Clean does before the mux matches', () => {
		assert.equal(normalisePath('/api//v1///editorial/queue'), '/api/v1/editorial/queue');
		assert.equal(normalisePath('/api/v1/%2Feditorial/queue'), '/api/v1/editorial/queue');
	});

	it('leaves an ordinary path exactly as it stands', () => {
		assert.equal(normalisePath('/el/munich+greece'), '/el/munich+greece');
		assert.equal(normalisePath('/api/v1/articles/9f0c'), '/api/v1/articles/9f0c');
	});

	// A path we cannot read is not a path we may guess at: the caller
	// refuses the request rather than routing a string it could not
	// resolve.
	it('reports an undecodable path as unreadable rather than guessing', () => {
		assert.equal(normalisePath('/api/v1/%ZZ'), null);
		assert.equal(normalisePath('/api/v1/%E0%A4%A'), null);
	});
});

describe('isRateLimitedApiPath', () => {
	it('covers the prefix the Go binary mounts editorial under', () => {
		assert.equal(EDITORIAL_PREFIX, '/api/v1/editorial/');
		assert.equal(isRateLimitedApiPath('/api/v1/editorial/queue'), true);
		assert.equal(isRateLimitedApiPath('/api/v1/editorial/'), true);
	});

	// The rule is inverted: everything under /api/ that is not provably a
	// reader path is limited, so a route added later is limited from its
	// first request rather than from the day somebody remembers it.
	it('limits an api path nobody has named yet', () => {
		assert.equal(isRateLimitedApiPath('/api/v1/whatever-lands-next'), true);
		assert.equal(isRateLimitedApiPath('/api/v2/front'), true);
		assert.equal(isRateLimitedApiPath('/api'), true);
	});

	it('lets the reader endpoints through: they are public data by design', () => {
		assert.equal(isRateLimitedApiPath('/api/v1/front'), false);
		assert.equal(isRateLimitedApiPath('/api/v1/articles/123'), false);
		assert.equal(isRateLimitedApiPath('/api/v1/openapi.json'), false);
		assert.equal(isReaderApiPath('/api/v1/front'), true);
	});

	// The release probe polls these on every deploy; a limited probe would
	// report a healthy deployment as a failed release.
	it('leaves the probe endpoints unlimited', () => {
		assert.equal(isRateLimitedApiPath('/healthz'), false);
		assert.equal(isRateLimitedApiPath('/readyz'), false);
	});

	it('says nothing about the pages, which the web container answers', () => {
		assert.equal(isRateLimitedApiPath('/el/editor'), false);
		assert.equal(isRateLimitedApiPath('/'), false);
		assert.equal(isRateLimitedApiPath('/apidocs'), false);
	});

	// The predicates are only sound on the string the router will read.
	// The prefix test the bypass walked past is the first assertion here;
	// the third is the case the inversion alone would not have caught,
	// because an encoded `/api` is not under `/api/` either.
	it('catches an encoded editorial path once it is normalised', () => {
		const raw = '/api/v1/%65ditorial/queue';
		assert.equal(raw.startsWith(EDITORIAL_PREFIX), false, 'what the prefix test missed');
		assert.equal(isRateLimitedApiPath(normalisePath(raw)), true);

		const encodedNamespace = '/%61pi/v1/editorial/queue';
		assert.equal(isRateLimitedApiPath(encodedNamespace), false, 'not under /api/ as written');
		assert.equal(isRateLimitedApiPath(normalisePath(encodedNamespace)), true);
		assert.equal(isApiPath(normalisePath(encodedNamespace)), true);
	});
});

/** A JWT-shaped token carrying these claims. Unsigned: nothing here verifies. */
const jwtWith = (claims) => {
	const encode = (value) =>
		Buffer.from(JSON.stringify(value))
			.toString('base64')
			.replace(/\+/g, '-')
			.replace(/\//g, '_')
			.replace(/=+$/, '');
	return `${encode({ alg: 'RS256', typ: 'JWT' })}.${encode(claims)}.c2lnbmF0dXJl`;
};

describe('rateLimitKey', () => {
	// The finding: API_BASE_URL is the deployment's own origin, so every
	// editorial call is made server-side by the web container and
	// cf-connecting-ip on that hop is the container's. Keyed on the
	// address, all editors share one bucket and 429 each other.
	it('counts a signed-in caller against their own subject, not the connection', () => {
		const request = new Request('https://news.example/api/v1/editorial/queue', {
			headers: {
				authorization: `Bearer ${jwtWith({ sub: 'eleni-uuid' })}`,
				'cf-connecting-ip': '203.0.113.7',
			},
		});
		assert.deepEqual(rateLimitKey(request), { key: 'sub:eleni-uuid', keyedOn: 'token' });
	});

	it('gives two editors behind one container address two buckets', () => {
		const from = (sub) =>
			rateLimitKey(
				new Request('https://news.example/api/v1/editorial/queue', {
					headers: {
						authorization: `Bearer ${jwtWith({ sub })}`,
						'cf-connecting-ip': '203.0.113.7',
					},
				}),
			).key;
		assert.notEqual(from('eleni-uuid'), from('dimitris-uuid'));
	});

	// Tokenless and unreadable-token traffic is the cheapest flood to
	// mount and the one this limit was written for; the address is what
	// bounds it.
	it('falls back to the address for a request carrying no usable token', () => {
		const from = (headers) =>
			rateLimitKey(new Request('https://news.example/api/v1/editorial/queue', { headers }));
		assert.deepEqual(from({ 'cf-connecting-ip': '203.0.113.7' }), {
			key: 'ip:203.0.113.7',
			keyedOn: 'address',
		});
		for (const authorization of [
			'Bearer not-a-jwt',
			'Basic ZWxlbmk6cHc=',
			'Bearer ',
			`Bearer ${jwtWith({ role: 'editor' })}`, // structurally fine, no subject
		]) {
			assert.deepEqual(
				from({ authorization, 'cf-connecting-ip': '203.0.113.7' }),
				{ key: 'ip:203.0.113.7', keyedOn: 'address' },
				authorization,
			);
		}
	});

	it('names the bucket "unknown" when there is no address either', () => {
		const key = rateLimitKey(new Request('https://news.example/api/v1/editorial/queue')).key;
		assert.equal(key, 'ip:unknown');
	});

	it('caps the key, so a caller cannot choose how long it is', () => {
		const request = new Request('https://news.example/api/v1/editorial/queue', {
			headers: { authorization: `Bearer ${jwtWith({ sub: 'x'.repeat(4096) })}` },
		});
		assert.equal(rateLimitKey(request).key.length, 'sub:'.length + RATE_LIMIT_KEY_MAX);
	});
});

describe('bearerToken and tokenSubject', () => {
	it('reads the credential of a Bearer header, whatever its case', () => {
		assert.equal(bearerToken('Bearer abc'), 'abc');
		assert.equal(bearerToken('bearer  abc  '), 'abc');
		assert.equal(bearerToken('Basic abc'), null);
		assert.equal(bearerToken(null), null);
		assert.equal(bearerToken(''), null);
	});

	it('reads the subject out of a JWT payload without verifying anything', () => {
		assert.equal(tokenSubject(jwtWith({ sub: 'eleni-uuid' })), 'eleni-uuid');
	});

	it('reports anything it cannot read as no subject at all', () => {
		assert.equal(tokenSubject(null), null);
		assert.equal(tokenSubject('a.b'), null);
		assert.equal(tokenSubject('a.@@@.c'), null);
		assert.equal(tokenSubject(jwtWith({ sub: 42 })), null);
		assert.equal(tokenSubject(jwtWith({})), null);
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
