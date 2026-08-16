import { defineMiddleware, sequence } from 'astro:middleware';

import { rememberEditorSession } from './lib/editorial/session';
import { resolveEditorSession } from './lib/editorial/supabase';
import { createUsageCounter } from './lib/usage';

// The single crawler enforcement point (FR-013, research D6). Three fences,
// one place, no per-route logic anywhere:
//
//   1. deny  — requests whose User-Agent matches the signature list get 403;
//   2. advise — /robots.txt is served disallow-all for every agent;
//   3. advise — every response carries `X-Robots-Tag: noindex, nofollow`,
//      covering compliant crawlers and cached copies, plus `Vary:
//      User-Agent` so shared caches never mix the 403 and page variants.
//
// The middleware ships inside the frontend container, so the block holds
// identically on Cloudflare Containers and Kubernetes. Platform-level bot
// management is additive config only — never the sole enforcement.
//
// Known limit, named openly: a crawler that fully impersonates a browser
// defeats origin-level User-Agent blocking; only platform-level heuristic
// bot management catches some of those.

/**
 * The maintained deny list of crawler/AI-training/archive bot signatures.
 *
 * Matching is a case-insensitive substring test against the request's
 * User-Agent, so each entry is the stable product token the bot declares.
 * Adding a bot is one line in the appropriate group.
 */
export const CRAWLER_SIGNATURES: readonly string[] = [
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

/** The `robots.txt` body: crawling disallowed everywhere, for every agent. */
export const ROBOTS_TXT_BODY = 'User-agent: *\nDisallow: /\n';

/** The advisory header value stamped on every response. */
export const X_ROBOTS_TAG_VALUE = 'noindex, nofollow';

/**
 * Reports whether a User-Agent value matches the deny list. A missing
 * User-Agent does not match: the list blocks declared crawlers, and ordinary
 * clients that omit the header must not be caught by it.
 */
export function matchesCrawlerSignature(userAgent: string | null): boolean {
  if (userAgent === null) {
    return false;
  }
  const normalised = userAgent.toLowerCase();
  return CRAWLER_SIGNATURES.some((signature) =>
    normalised.includes(signature.toLowerCase()),
  );
}

/** Fence 1 — deny: matching User-Agents receive an actual 403, not advice. */
const denyCrawlers = defineMiddleware((context, next) => {
  if (matchesCrawlerSignature(context.request.headers.get('user-agent'))) {
    return new Response('Forbidden', { status: 403 });
  }
  return next();
});

/** Fence 2 — advise: serve the disallow-all `robots.txt` from here. */
const serveRobotsTxt = defineMiddleware((context, next) => {
  if (context.request.method === 'GET' && context.url.pathname === '/robots.txt') {
    return new Response(ROBOTS_TXT_BODY, {
      status: 200,
      headers: { 'Content-Type': 'text/plain; charset=utf-8' },
    });
  }
  return next();
});

/**
 * Appends `User-Agent` to the response's `Vary` header without clobbering
 * an existing value. Responses at the same URL differ by User-Agent (403
 * versus the page), so a shared cache must never serve one variant to the
 * other audience.
 */
function appendVaryUserAgent(headers: Headers): void {
  const existing = headers.get('vary');
  if (existing === null || existing.trim() === '') {
    headers.set('Vary', 'User-Agent');
    return;
  }
  const fields = existing.split(',').map((field) => field.trim().toLowerCase());
  if (fields.includes('*') || fields.includes('user-agent')) {
    return;
  }
  headers.set('Vary', `${existing}, User-Agent`);
}

/**
 * Fence 3 — advise: stamp `X-Robots-Tag` and `Vary: User-Agent` on every
 * response. Runs first in the sequence so the headers also land on the 403s
 * and on `robots.txt`.
 */
const stampAdvisoryHeader = defineMiddleware(async (_context, next) => {
  const response = await next();
  response.headers.set('X-Robots-Tag', X_ROBOTS_TAG_VALUE);
  appendVaryUserAgent(response.headers);
  return response;
});

/**
 * Whether a path is one of the editorial screens, `/{lang}/editor…`.
 *
 * Deliberately a path test rather than a route test: resolving an
 * identity costs a round trip to the auth server, and the reader pages —
 * which are the overwhelming majority of requests — must not pay it.
 */
export function isEditorialPath(pathname: string): boolean {
  return /^\/[^/]+\/editor(?:\/|$)/.test(pathname);
}

/**
 * Resolves the editor identity once per request, before anything renders.
 *
 * The screens read it back through `editorSession()`. It happens here
 * rather than in each page because resolving a session can refresh the
 * access token, and only middleware can write the new one back to the
 * browser — a page that cannot persist a refresh signs the editor out
 * roughly every hour.
 *
 * The response is marked uncacheable for the same reason auth cookies
 * are: an editorial page carries one named person's session, and a shared
 * cache handing it to someone else would put the wrong name beside the
 * word "approver".
 */
const resolveEditorIdentity = defineMiddleware(async (context, next) => {
  if (!isEditorialPath(context.url.pathname)) {
    return next();
  }
  rememberEditorSession(
    context.request,
    await resolveEditorSession(context.request, context.cookies),
  );
  const response = await next();
  response.headers.set('Cache-Control', 'private, no-store');
  return response;
});

/**
 * Aggregate usage counting (issue #91): one in-memory counter per server
 * process, flushed as a `usage_rollup` structured log line on the request
 * that finds the interval elapsed. Outermost in the sequence so the
 * counted status is the one actually sent — crawler 403s and robots.txt
 * hits included. The counter's API receives only the pathname and status;
 * IPs, User-Agents and cookies never reach it.
 */
const usageCounter = createUsageCounter();

const countUsage = defineMiddleware(async (context, next) => {
  const response = await next();
  const now = new Date();
  usageCounter.record(now, context.url.pathname, response.status);
  usageCounter.flushIfDue(now, (rollup) => {
    console.log(JSON.stringify(rollup));
  });
  return response;
});

// Usage counting stays outermost so the status it records is the one
// actually sent; identity resolution stays innermost so a crawler's 403
// never costs an auth round trip.
export const onRequest = sequence(
  countUsage,
  stampAdvisoryHeader,
  denyCrawlers,
  serveRobotsTxt,
  resolveEditorIdentity,
);
