// Cloudflare deployment shim — a deployment artefact, not application code.
//
// Cloudflare Containers are hosted by Durable Object classes and reached
// exclusively through a Worker; this file is that minimal glue, free of any
// dependency beyond the built-in "cloudflare:workers" module and the pure
// routing rules next door. The containers themselves are the ordinary images
// built from ./Dockerfile and ./web/Dockerfile — they know nothing about
// Cloudflare.
//
// Topology (issue #134): the Worker is the single public entry point and
// routes by path. `/api/…`, `/healthz` and `/readyz` go to the API
// container; everything else goes to the web container. Cloudflare
// containers share no private network — a container is reachable only
// through its Durable Object, i.e. from Worker code — so the public origin
// is the only address the web container can use for the api, and the
// reader endpoints are public data by design. The editorial endpoints stay
// JWT-gated, the database enforces the editor role a second time, and the
// Worker rate-limits their prefix because public reachability makes
// invalid-token load possible.
//
// Two more things happen on the way through, both invisible until a real
// browser met a real deploy (see routing.js for the reasoning): the proxy
// hop is plain HTTP, and the site's own Origin/Referer are translated to
// match so that form posts are not all refused as cross-site.

import { DurableObject } from "cloudflare:workers";

import {
	containerRequest,
	isApiPath,
	isEditorialPath,
	withRobotsTag,
} from "./routing.js";

/**
 * Hosts one container: boots it on first request and proxies HTTP to it.
 */
class ContainerHost extends DurableObject {
	/** Port the container process listens on. Subclasses override. */
	port = 8080;

	/**
	 * Environment handed to the container at start. Subclasses override.
	 * Secrets arrive here through `this.env`, populated by
	 * `npx wrangler secret put <NAME>` — values never live in the repo.
	 *
	 * It receives the request that woke the container, because one value —
	 * the deployment's own public origin — is knowable only from a request
	 * that arrived on it (see WebContainer).
	 */
	containerEnv(_request) {
		return {};
	}

	async fetch(request) {
		if (!this.ctx.container.running) {
			this.ctx.container.start({
				// The original request, before the proxy rewrite below: the
				// public origin is what the browser asked for, not the
				// http:// address the container is reached at.
				env: this.containerEnv(request),
				// Outbound only: feeds, Supabase, translation provider, and
				// — since the api has no private address — the web
				// container's own calls back through the public origin.
				enableInternet: true,
			});
		}
		const port = this.ctx.container.getTcpPort(this.port);
		// The container speaks plain HTTP and knows nothing of the TLS hop
		// in front of it; containerRequest is where the scheme and the
		// same-site Origin/Referer are translated.
		let proxied = containerRequest(request);
		// start() returns before the process accepts connections; retry
		// briefly rather than failing the first request after a cold start.
		// A request body is a one-shot stream, so every attempt holds a
		// clone taken before fetching — a failed attempt never leaves the
		// next one without an unconsumed body.
		let lastError;
		for (let attempt = 0; attempt < 10; attempt++) {
			const reserve = proxied.clone();
			try {
				return await port.fetch(proxied);
			} catch (error) {
				lastError = error;
				proxied = reserve;
				await scheduler.wait(500);
			}
		}
		throw lastError;
	}
}

/** Go API (./Dockerfile), reached at `/api/…`, `/healthz` and `/readyz`. */
export class ApiContainer extends ContainerHost {
	port = 8080; // matches HTTP_ADDR below

	containerEnv() {
		// Fail fast with a named cause: the Go binary requires DATABASE_URL
		// and would otherwise exit-loop behind opaque connection retries.
		if (!this.env.DATABASE_URL) {
			throw new Error(
				"DATABASE_URL secret is not set; run `npx wrangler secret put DATABASE_URL`",
			);
		}
		const containerEnv = {
			DATABASE_URL: this.env.DATABASE_URL, // Cloudflare secret
			HTTP_ADDR: ":8080",
			APP_ENV: "prod",
			LOG_LEVEL: "info",
		};
		// JWT verification for the editorial endpoints. Plain vars, not
		// secrets: JWKS_URL names where PUBLIC keys are published. Missing
		// JWKS_URL is deliberately NOT fatal here — the reader path needs no
		// token, so an editorial-only misconfiguration must not take the
		// public site down. The Go binary logs an ERROR line at startup and
		// leaves every /api/v1/editorial/ route unmounted (404).
		//
		// Each var is forwarded INDEPENDENTLY. Gating the audience on the
		// JWKS URL would quietly drop a configured JWT_AUDIENCE whenever
		// JWKS_URL was absent — exactly the inconsistent pair that
		// config.FromEnv is written to reject. Swallowing it here would
		// convert a loud, documented startup error into a silent degrade to
		// reader-only mode, hiding the operator's mistake.
		if (this.env.JWKS_URL) {
			containerEnv.JWKS_URL = this.env.JWKS_URL;
		}
		if (this.env.JWT_AUDIENCE) {
			containerEnv.JWT_AUDIENCE = this.env.JWT_AUDIENCE;
		}
		// Feed poll loop cadence. Forwarded explicitly like everything
		// above — a var declared in wrangler.jsonc that this method does
		// not copy never reaches the container. Unset means the binary's
		// documented 15m default, and "0" (a truthy string, so it does get
		// forwarded) disables the loop — the only disable switch.
		if (this.env.POLL_INTERVAL) {
			containerEnv.POLL_INTERVAL = this.env.POLL_INTERVAL;
		}
		// Machine translation (T018): the provider, its prices and the
		// FR-006 budget. Forwarded independently and only when set, like
		// everything above — a var this method does not copy never
		// reaches the container. All of them unset is the documented off
		// state (the binary logs one line and translates nothing); the
		// binary itself names any half-configured set at startup, so
		// nothing is gated on anything else here. TRANSLATION_API_KEY is
		// a Cloudflare secret (`npx wrangler secret put
		// TRANSLATION_API_KEY`); the rest are plain vars in
		// wrangler.jsonc. NO DEFAULTS anywhere in this chain: the
		// provider and the budget are a founder decision, and a value
		// invented here would make it by accident.
		for (const name of [
			"TRANSLATION_BASE_URL",
			"TRANSLATION_MODEL",
			"TRANSLATION_API_KEY",
			"TRANSLATION_INPUT_USD_PER_MTOK",
			"TRANSLATION_OUTPUT_USD_PER_MTOK",
			"TRANSLATION_FREE_OF_CHARGE",
			"TRANSLATION_INTERVAL",
			"TRANSLATION_ARTICLE_CEILING_MICROUSD",
			"TRANSLATION_MONTHLY_CAP_MICROUSD",
		]) {
			if (this.env[name]) {
				containerEnv[name] = this.env[name];
			}
		}
		return containerEnv;
	}
}

/** Astro web frontend (./web/Dockerfile) — every path but the API's. */
export class WebContainer extends ContainerHost {
	port = 4321; // Astro node-adapter default, pinned via PORT below

	containerEnv(request) {
		const containerEnv = {
			HOST: "0.0.0.0",
			PORT: "4321",
			// A Cloudflare deployment is a deployed environment, and the
			// frontend must know it: in prod the reader refuses to answer
			// from its built-in fixtures rather than presenting invented
			// publishers and an invented approver as the record
			// (web/src/lib/reader/api.ts).
			APP_ENV: "prod",
		};
		// Everything below is forwarded INDEPENDENTLY, and only when set.
		// A var declared in wrangler.jsonc that this method does not copy
		// never reaches the container at all: the platform hands the
		// container exactly this object. Silence is the failure mode —
		// the Astro server starts, serves its built-in fixtures and looks
		// perfectly healthy while showing nobody's real data.
		//
		// API_BASE_URL: where the pages find the Go api. Cloudflare
		// containers share no private network, so the deployment's own
		// public origin is the only address that reaches the api — the
		// Worker routes /api/… there (issue #134).
		//
		// It is DERIVED from the request that started this container
		// rather than written into wrangler.jsonc, because the origin is
		// not knowable when the file is written: a workers.dev hostname
		// contains the account subdomain, and a committed guess would be
		// an invented value of exactly the kind this product exists to
		// refuse. The request that arrived states the origin as fact.
		//
		// An explicit var wins, for a deployment behind a custom domain
		// that must be named rather than observed: with two hostnames in
		// front of one deployment, whichever request wakes the container
		// would otherwise pin the origin for its lifetime.
		containerEnv.API_BASE_URL =
			this.env.API_BASE_URL || new URL(request.url).origin;
		// Supabase Auth for the editorial sign-in. Plain vars, not
		// secrets: the anon key identifies the project and grants nothing
		// on its own. Missing either one is deliberately not fatal — the
		// reader path needs no sign-in, so an editorial-only gap must not
		// take the public site down. The sign-in page states that no auth
		// is configured rather than offering a form that could only fail.
		if (this.env.PUBLIC_SUPABASE_URL) {
			containerEnv.PUBLIC_SUPABASE_URL = this.env.PUBLIC_SUPABASE_URL;
		}
		if (this.env.PUBLIC_SUPABASE_ANON_KEY) {
			containerEnv.PUBLIC_SUPABASE_ANON_KEY = this.env.PUBLIC_SUPABASE_ANON_KEY;
		}
		// The release version (issue #119), set per deployment by the
		// release workflow (`wrangler deploy --var PUBLIC_APP_VERSION:vX.Y.Z`)
		// and rendered in the footer's fine print. Forwarded only when set:
		// a deployment nobody released through a tag shows no version, and
		// nothing here may invent one.
		if (this.env.PUBLIC_APP_VERSION) {
			containerEnv.PUBLIC_APP_VERSION = this.env.PUBLIC_APP_VERSION;
		}
		return containerEnv;
	}
}

/**
 * The 429 the rate limiter answers with, in the API's own error vocabulary
 * (internal/platform/http/problem.go) so a client sees one shape whether
 * the refusal came from the Worker or from the Go binary.
 */
function tooManyRequests(detail) {
	return new Response(
		JSON.stringify({
			type: "about:blank",
			title: "Too Many Requests",
			status: 429,
			detail,
		}),
		{
			status: 429,
			headers: {
				"Content-Type": "application/problem+json",
				"Retry-After": "60",
			},
		},
	);
}

/**
 * Applies the editorial rate limit, returning a refusal or null.
 *
 * Keyed on the client IP: the point is to bound how much token
 * verification a stranger can make us do, and a stranger has no account to
 * key on. An authenticated editor works well inside the limit — the
 * screens make a handful of requests per action.
 *
 * A missing binding refuses rather than passes. Serving the editorial
 * prefix unlimited while believing it limited is the silent state this
 * whole issue is about; the reader path is untouched by the refusal, which
 * is the same trade JWKS_URL already makes.
 */
async function limitEditorial(request, env) {
	if (!env.EDITORIAL_RATE_LIMIT) {
		return tooManyRequests(
			"the editorial rate limiter is not configured on this deployment, so the editorial endpoints are refused; the EDITORIAL_RATE_LIMIT binding is declared in wrangler.jsonc",
		);
	}
	const key = request.headers.get("cf-connecting-ip") ?? "unknown";
	const { success } = await env.EDITORIAL_RATE_LIMIT.limit({ key });
	if (success) {
		return null;
	}
	return tooManyRequests(
		"too many requests to the editorial endpoints from this address; retry shortly",
	);
}

export default {
	/**
	 * The only public entry point. Path decides the container: the api
	 * answers its own namespace and the probe endpoints, the web container
	 * answers everything else.
	 */
	async fetch(request, env) {
		const { pathname } = new URL(request.url);
		if (isEditorialPath(pathname)) {
			const refusal = await limitEditorial(request, env);
			if (refusal !== null) {
				return refusal;
			}
		}
		const container = isApiPath(pathname)
			? env.API.get(env.API.idFromName("api"))
			: env.WEB.get(env.WEB.idFromName("web"));
		return withRobotsTag(await container.fetch(request));
	},
};
