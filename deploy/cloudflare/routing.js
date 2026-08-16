// The Worker's request-shaping rules, kept apart from the Worker itself.
//
// Everything here is a pure function over Request/Response/Headers/URL — the
// standard web objects, and nothing from "cloudflare:workers". That is the
// whole point of the split: these rules decide which container answers, what
// the container is told about the caller, and what the caller is told about
// indexing, and all three were previously invisible to every test we have.
// `node --test deploy/cloudflare/routing.test.mjs` exercises them directly;
// worker.js imports them and adds only the platform glue.

/** The advisory value stamped on responses that carry none of their own. */
export const X_ROBOTS_TAG_VALUE = 'noindex, nofollow';

/**
 * The crawler deny list, kept identical to `CRAWLER_SIGNATURES` in
 * web/src/middleware.ts.
 *
 * Two copies, on purpose and with a test to keep them equal
 * (routing.test.mjs). The web container's list is the one that ships
 * inside the artefact, so the fence holds identically on Kubernetes where
 * no Worker exists — it cannot move here. But the Worker now routes
 * /api/… to the api container, which has no such middleware, so a
 * declared crawler could read the reader endpoints as JSON without ever
 * meeting the fence the pages are behind (FR-013, research D6). The edge
 * copy closes that, and refuses before a container is even woken.
 *
 * Adding a bot means adding it in both places; the drift test says so by
 * name when only one is updated.
 */
export const CRAWLER_SIGNATURES = [
	// AI training / assistant crawlers
	'GPTBot',
	'ClaudeBot',
	'CCBot',
	'Google-Extended',
	'Bytespider',
	'PerplexityBot',
	'Amazonbot',
	'meta-externalagent',
	// Archive crawlers
	'ia_archiver',
	'archive.org_bot',
	// Search engine crawlers
	'Googlebot',
	'Bingbot',
	'DuckDuckBot',
	'Baiduspider',
	'YandexBot',
	'Applebot',
];

/**
 * Whether a User-Agent matches the deny list. A missing User-Agent does
 * not match: the list blocks crawlers that declare themselves, and an
 * ordinary client that omits the header must not be caught by it.
 */
export function matchesCrawlerSignature(userAgent) {
	if (userAgent === null || userAgent === undefined) {
		return false;
	}
	const normalised = userAgent.toLowerCase();
	return CRAWLER_SIGNATURES.some((signature) =>
		normalised.includes(signature.toLowerCase()),
	);
}

/**
 * The crawler refusal: an actual 403, not advice.
 *
 * `Vary: User-Agent` because the same URL answers differently by
 * User-Agent, and a shared cache must never hand one audience the other's
 * response — the same reason the web container's middleware sets it.
 */
export function crawlerRefusal() {
	return new Response('Forbidden', {
		status: 403,
		headers: {
			'Content-Type': 'text/plain; charset=utf-8',
			'Vary': 'User-Agent',
			'X-Robots-Tag': X_ROBOTS_TAG_VALUE,
		},
	});
}

/** Where the editorial module's routes live (cmd/apivo/main.go). */
export const EDITORIAL_PREFIX = '/api/v1/editorial/';

/**
 * The path as the Go router will read it, or null if it cannot be read at
 * all.
 *
 * The Worker sees the raw, percent-encoded pathname; Go's ServeMux matches
 * after unescaping each segment and after cleaning the path. So
 * `/api/v1/%65ditorial/queue` was not editorial to this file and was
 * editorial to the api — one encoded character walked past the rate limit
 * and reached the handler. Deciding anything on the raw pathname is
 * deciding on a different string from the one that routes.
 *
 * Decoded ONCE, like the router: `%2565` decodes to `%65`, which is not
 * `e` to Go either, so a second pass would invent a match nobody will
 * serve. Repeated slashes are collapsed because `path.Clean` does the
 * same before the mux matches.
 *
 * A pathname that does not decode returns null and the caller refuses the
 * request. An undecodable path is not a request we owe a best effort: we
 * cannot say what it addresses, and guessing is how the encoded-path
 * bypass happened in the first place.
 */
export function normalisePath(pathname) {
	let decoded;
	try {
		decoded = decodeURIComponent(pathname);
	} catch {
		return null;
	}
	return decoded.replace(/\/{2,}/g, '/');
}

/**
 * The reader endpoints: public data by design, answered without a token,
 * and the only paths under `/api/` that the limiter lets through.
 *
 * They are listed positively (cmd/apivo/main.go, internal/content/http.go)
 * because the rule below is inverted: everything else under `/api/` is
 * limited. A route added to the API later is limited from its first
 * request rather than from the day somebody remembers to name it here,
 * which is the failure direction worth having.
 */
export const READER_PATHS = ['/api/v1/front', '/api/v1/openapi.json'];

/** Reader endpoints identified by prefix — `/api/v1/articles/{id}`. */
export const READER_PREFIXES = ['/api/v1/articles/'];

/** Whether this normalised path is one of the public reader endpoints. */
export function isReaderApiPath(path) {
	return (
		READER_PATHS.includes(path) ||
		READER_PREFIXES.some((prefix) => path.startsWith(prefix))
	);
}

/**
 * Whether this path is answered by the Go API container rather than the
 * Astro web container.
 *
 * The API's whole public surface is `/api/...` plus the two probe endpoints.
 * `/healthz` and `/readyz` belong to the api because the api is what a
 * release probe must ask about: it is the process that migrated the schema
 * and holds the database pool, so its verdict is the deployment's verdict
 * (.github/workflows/release.yml).
 *
 * Bare `/api` is included so a request that misses the trailing slash still
 * reaches the API and receives its problem+json 404, rather than the web
 * container's HTML one.
 */
export function isApiPath(pathname) {
	return (
		pathname === '/healthz' ||
		pathname === '/readyz' ||
		pathname === '/api' ||
		pathname.startsWith('/api/')
	);
}

/**
 * Whether this normalised path is rate-limited.
 *
 * The rule is INVERTED on purpose: everything the api answers is limited
 * EXCEPT the reader endpoints and the two probes. The editorial prefix is
 * what the limit was written for — routing `/api/*` publicly makes it
 * reachable by anyone, and an invalid bearer token still costs a
 * verification — but a rule shaped as "limit this one prefix" fails open
 * for every path nobody thought of, and the encoded-path bypass was
 * exactly that shape of mistake. Shaped this way, an unrecognised path
 * under `/api/` is limited, and the cost of being wrong is a stranger
 * waiting rather than a stranger unbounded.
 *
 * `/healthz` and `/readyz` stay out: the release probe polls them on every
 * deploy (.github/workflows/release.yml) and a rate-limited probe would
 * report a healthy deployment as a failed release. They take no argument
 * and touch no token.
 *
 * The caller must pass a normalised path (see normalisePath) — this is
 * the predicate the encoded-path bypass walked past.
 */
export function isRateLimitedApiPath(path) {
	if (path === '/healthz' || path === '/readyz') {
		return false;
	}
	if (!isApiPath(path)) {
		return false;
	}
	return !isReaderApiPath(path);
}

/**
 * Rewrites `Origin` and `Referer` from `https://<host>` to `http://<host>`
 * FOR THIS SITE'S OWN HOST ONLY, in place.
 *
 * The containers speak plain HTTP: the request they receive from the Worker
 * has an `http://` URL, so the web container's same-origin check
 * (web/src/lib/csrf.ts, called with `Astro.url.origin`) compares the
 * browser's `https://<host>` against its own `http://<host>` and refuses
 * every form post — sign-in, approval, withdrawal, source registration.
 *
 * The rewrite is deliberately host-exact and never touches a foreign
 * origin. That is what preserves the CSRF property: a post from
 * `https://evil.example` arrives at the container still saying
 * `https://evil.example`, matches nothing, and is refused exactly as
 * before. Rewriting the scheme of whatever turned up would hand every
 * attacker the same-origin answer.
 *
 * @param {Headers} headers Mutated in place.
 * @param {string} siteHost The request's own host, e.g. "news.example".
 */
export function rewriteSameSiteOriginHeaders(headers, siteHost) {
	const siteOrigin = `https://${siteHost}`;
	if (headers.get('origin') === siteOrigin) {
		headers.set('Origin', `http://${siteHost}`);
	}
	const referer = headers.get('referer');
	if (referer === null || referer === '') {
		return headers;
	}
	let parsed;
	try {
		parsed = new URL(referer);
	} catch {
		// An unparseable Referer is left exactly as it arrived: it matches
		// no origin, which is the refusal the check already gives it.
		return headers;
	}
	if (parsed.origin === siteOrigin) {
		parsed.protocol = 'http:';
		headers.set('Referer', parsed.toString());
	}
	return headers;
}

/** The header the public scheme is stated in, for the container. */
export const FORWARDED_PROTO_HEADER = 'X-Forwarded-Proto';

/**
 * The request as the container must see it: same method, body and headers,
 * but over `http://`, with the site's own `Origin`/`Referer` translated to
 * match and the real public scheme stated in `X-Forwarded-Proto`.
 *
 * `new Request(url, request)` is the platform idiom for "this request, at
 * another URL" — it carries method, headers and the body stream across
 * without consuming it.
 */
export function containerRequest(request) {
	const url = new URL(request.url);
	const siteHost = url.host;
	const publicProtocol = url.protocol;
	// The containers listen on plain HTTP inside the Durable Object; the
	// public hop is what carries TLS. Proxying the browser's https:// URL
	// unchanged asks the container to speak a protocol it does not.
	url.protocol = 'http:';
	const proxied = new Request(url, request);
	// …and having rewritten it, say what it was. The container cannot
	// otherwise know: @astrojs/node builds `Astro.url` from the socket and
	// the Host header and ignores this header, so a cookie whose `Secure`
	// followed the request URL lost it on every https deployment
	// (web/src/lib/secure-request.ts).
	//
	// SET, never appended: whatever a client sent under this name is
	// overwritten here, which is the only reason the container may trust
	// it. This Worker is the sole route to the container.
	proxied.headers.set(FORWARDED_PROTO_HEADER, publicProtocol.replace(':', ''));
	rewriteSameSiteOriginHeaders(proxied.headers, siteHost);
	return proxied;
}

/**
 * Adds `User-Agent` to a response's `Vary`, in place, without displacing
 * what is already there.
 *
 * Every response this Worker emits depends on the User-Agent, because the
 * crawler fence answers 403 to some callers and passes the rest through.
 * Declaring that only on the refusal was half the statement: a shared
 * cache that stored the allowed response could hand it to a denied
 * crawler, or hand a reader the denial.
 *
 * `Vary: *` is left alone — it already says "vary on everything", and
 * appending to it would only make it less true.
 *
 * @param {Headers} headers Mutated in place.
 */
export function varyOnUserAgent(headers) {
	const existing = headers.get('vary');
	if (existing === null || existing.trim() === '') {
		headers.set('Vary', 'User-Agent');
		return headers;
	}
	if (existing.trim() === '*') {
		return headers;
	}
	const fields = existing.split(',').map((field) => field.trim().toLowerCase());
	if (!fields.includes('user-agent')) {
		headers.set('Vary', `${existing}, User-Agent`);
	}
	return headers;
}

/**
 * The HSTS policy: a year, subdomains included, `preload` deliberately
 * absent.
 *
 * A year is the value the preload list requires and what every guide
 * recommends; `includeSubDomains` because this deployment is the whole of
 * its hostname and nothing else lives under it. `preload` is NOT here: it
 * is a submission to a browser-vendor list and a commitment that outlives
 * the domain, and a header cannot make that claim on the founder's
 * behalf.
 */
export const HSTS_VALUE = 'max-age=31536000; includeSubDomains';

/**
 * Stamps the headers the edge owes every response: the indexing advisory,
 * the User-Agent cache variance, and — on an https request — the HSTS
 * policy.
 *
 * `X-Robots-Tag`: the crawler fences live in the web container's
 * middleware (FR-013, research D6), which never runs for the static assets
 * the Astro node adapter serves itself — so images, CSS and JS left the
 * origin with no advisory header at all. Stamping here closes that gap for
 * every byte the deployment emits, api container included.
 *
 * `Strict-Transport-Security`: `workers.dev` is under a HSTS-preloaded TLD,
 * so a browser upgrades those requests before they leave. A CUSTOM DOMAIN,
 * which docs/RELEASING.md names as a supported target for this same
 * deploy, has nothing supplying that — the first visit is plain http and a
 * cookie can ride it. The Worker is the only place that knows the public
 * scheme, so it is where the policy is stated. Only on an https request:
 * over cleartext the header is meaningless (RFC 6797 has the agent ignore
 * it) and stating a policy on a hop that proves nothing is not a claim
 * worth making.
 *
 * `Vary`: see varyOnUserAgent — the crawler decision applies to every
 * response, so every response must declare that it does.
 *
 * The other two only fill a gap: a response that already states its own
 * value keeps it, so the origin stays the authority and this is a floor
 * beneath it, not an override of it.
 *
 * @param response The container's (or a refusal's) response.
 * @param secure Whether the PUBLIC hop was https - not the proxy hop.
 */
export function withEdgeHeaders(response, secure) {
	// A WebSocket upgrade has no reconstructable response; nothing about it
	// is indexable either.
	if (response.webSocket) {
		return response;
	}
	const stamped = new Response(response.body, response);
	varyOnUserAgent(stamped.headers);
	if (!stamped.headers.has('x-robots-tag')) {
		stamped.headers.set('X-Robots-Tag', X_ROBOTS_TAG_VALUE);
	}
	if (secure && !stamped.headers.has('strict-transport-security')) {
		stamped.headers.set('Strict-Transport-Security', HSTS_VALUE);
	}
	return stamped;
}
