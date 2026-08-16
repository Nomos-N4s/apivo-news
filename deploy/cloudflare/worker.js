// Cloudflare deployment shim — a deployment artefact, not application code.
//
// Cloudflare Containers are hosted by Durable Object classes and reached
// exclusively through a Worker; this file is that minimal glue, free of any
// dependency beyond the built-in "cloudflare:workers" module. The containers
// themselves are the ordinary images built from ./Dockerfile and
// ./web/Dockerfile — they know nothing about Cloudflare.
//
// Topology (specs/001-epiloyes-alpha/contracts/http-api.md): every public
// request goes to the web container. The API container has no public route:
// its class exists so the platform builds, pins (EU jurisdiction,
// wrangler.jsonc) and runs the image, and it is addressable solely through
// the API binding from Worker code.

import { DurableObject } from "cloudflare:workers";

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
	 */
	containerEnv() {
		return {};
	}

	async fetch(request) {
		if (!this.ctx.container.running) {
			this.ctx.container.start({
				env: this.containerEnv(),
				// Outbound only: feeds, Supabase, translation provider.
				enableInternet: true,
			});
		}
		const port = this.ctx.container.getTcpPort(this.port);
		// start() returns before the process accepts connections; retry
		// briefly rather than failing the first request after a cold start.
		// A request body is a one-shot stream, so every attempt holds a
		// clone taken before fetching — a failed attempt never leaves the
		// next one without an unconsumed body.
		let lastError;
		for (let attempt = 0; attempt < 10; attempt++) {
			const reserve = request.clone();
			try {
				return await port.fetch(request);
			} catch (error) {
				lastError = error;
				request = reserve;
				await scheduler.wait(500);
			}
		}
		throw lastError;
	}
}

/** Go API (./Dockerfile). Internal only — nothing routes here publicly. */
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

/** Astro web frontend (./web/Dockerfile) — the single public surface. */
export class WebContainer extends ContainerHost {
	port = 4321; // Astro node-adapter default, pinned via PORT below

	containerEnv() {
		const containerEnv = {
			HOST: "0.0.0.0",
			PORT: "4321",
		};
		// Everything below is forwarded INDEPENDENTLY, and only when set.
		// A var declared in wrangler.jsonc that this method does not copy
		// never reaches the container at all: the platform hands the
		// container exactly this object. Silence is the failure mode —
		// the Astro server starts, serves its built-in fixtures and looks
		// perfectly healthy while showing nobody's real data.
		//
		// API_BASE_URL: where the pages find the Go api. There is no
		// in-platform value for it today — Cloudflare Containers are
		// reachable only through their Durable Object binding, i.e. from
		// Worker code, so the api container has no address the web
		// container could fetch, and the public hostname routes to the
		// web container itself. It is forwarded so a deployment whose api
		// runs somewhere addressable can point at it; left empty the
		// pages render from fixtures.
		if (this.env.API_BASE_URL) {
			containerEnv.API_BASE_URL = this.env.API_BASE_URL;
		}
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
		return containerEnv;
	}
}

export default {
	/** The only public entry point: everything goes to the web container. */
	async fetch(request, env) {
		const web = env.WEB.get(env.WEB.idFromName("web"));
		return web.fetch(request);
	},
};
