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

/** Where the editorial module's routes live (cmd/apivo/main.go). */
export const EDITORIAL_PREFIX = '/api/v1/editorial/';

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
 * Whether this path is an editorial endpoint — the rate-limited prefix.
 *
 * Routing `/api/*` publicly makes the editorial endpoints reachable by
 * anyone, and an invalid bearer token still costs a JWKS verification and a
 * database round trip. The token gate is unchanged; this only bounds how
 * fast a stranger may make us check one.
 */
export function isEditorialPath(pathname) {
	return pathname.startsWith(EDITORIAL_PREFIX);
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

/**
 * The request as the container must see it: same method, body and headers,
 * but over `http://` and with the site's own `Origin`/`Referer` translated
 * to match.
 *
 * `new Request(url, request)` is the platform idiom for "this request, at
 * another URL" — it carries method, headers and the body stream across
 * without consuming it.
 */
export function containerRequest(request) {
	const url = new URL(request.url);
	const siteHost = url.host;
	// The containers listen on plain HTTP inside the Durable Object; the
	// public hop is what carries TLS. Proxying the browser's https:// URL
	// unchanged asks the container to speak a protocol it does not.
	url.protocol = 'http:';
	const proxied = new Request(url, request);
	rewriteSameSiteOriginHeaders(proxied.headers, siteHost);
	return proxied;
}

/**
 * Stamps `X-Robots-Tag` on a response that does not already carry one.
 *
 * The crawler fences live in the web container's middleware (FR-013,
 * research D6), which never runs for the static assets the Astro node
 * adapter serves itself — so images, CSS and JS left the origin with no
 * advisory header at all. Stamping here closes that gap for every byte the
 * deployment emits, api container included.
 *
 * It only fills a gap: a response that already states its own indexing
 * rules keeps them, so the origin stays the authority and this is a floor
 * beneath it, not an override of it.
 */
export function withRobotsTag(response) {
	// A WebSocket upgrade has no reconstructable response; nothing about it
	// is indexable either.
	if (response.webSocket) {
		return response;
	}
	if (response.headers.has('x-robots-tag')) {
		return response;
	}
	const stamped = new Response(response.body, response);
	stamped.headers.set('X-Robots-Tag', X_ROBOTS_TAG_VALUE);
	return stamped;
}
