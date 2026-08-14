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
		return {
			DATABASE_URL: this.env.DATABASE_URL, // Cloudflare secret
			HTTP_ADDR: ":8080",
			APP_ENV: "prod",
			LOG_LEVEL: "info",
		};
	}
}

/** Astro web frontend (./web/Dockerfile) — the single public surface. */
export class WebContainer extends ContainerHost {
	port = 4321; // Astro node-adapter default, pinned via PORT below

	containerEnv() {
		return {
			HOST: "0.0.0.0",
			PORT: "4321",
		};
	}
}

export default {
	/** The only public entry point: everything goes to the web container. */
	async fetch(request, env) {
		const web = env.WEB.get(env.WEB.idFromName("web"));
		return web.fetch(request);
	},
};
