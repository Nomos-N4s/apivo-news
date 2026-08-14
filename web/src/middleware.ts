import { defineMiddleware, sequence } from 'astro:middleware';

// The single crawler enforcement point (FR-013, research D6). Three fences,
// one place, no per-route logic anywhere:
//
//   1. deny  — requests whose User-Agent matches the signature list get 403;
//   2. advise — /robots.txt is served disallow-all for every agent;
//   3. advise — every response carries `X-Robots-Tag: noindex, nofollow`,
//      covering compliant crawlers and cached copies.
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
 * Fence 3 — advise: stamp `X-Robots-Tag` on every response. Runs first in
 * the sequence so the header also lands on the 403s and on `robots.txt`.
 */
const stampAdvisoryHeader = defineMiddleware(async (_context, next) => {
  const response = await next();
  response.headers.set('X-Robots-Tag', X_ROBOTS_TAG_VALUE);
  return response;
});

export const onRequest = sequence(stampAdvisoryHeader, denyCrawlers, serveRobotsTxt);
