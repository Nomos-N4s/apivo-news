import { describe, expect, it, vi } from 'vitest';
import type { APIContext, MiddlewareNext } from 'astro';
import {
  CRAWLER_SIGNATURES,
  ROBOTS_TXT_BODY,
  X_ROBOTS_TAG_VALUE,
  matchesCrawlerSignature,
  onRequest,
} from './middleware';

/** Builds the minimal request context the middleware reads. */
function makeContext(options: {
  path?: string;
  method?: string;
  userAgent?: string;
}): APIContext {
  const url = new URL(options.path ?? '/', 'http://localhost:4321');
  const headers = new Headers();
  if (options.userAgent !== undefined) {
    headers.set('user-agent', options.userAgent);
  }
  const context = {
    request: new Request(url, { method: options.method ?? 'GET', headers }),
    url,
  };
  // The middleware only reads `request` and `url`; the rest of APIContext is
  // render-pipeline state that a unit test neither needs nor can construct.
  return context as APIContext;
}

/** A `next` that renders a plain page and records whether it was reached. */
function makeNext(): { next: MiddlewareNext; wasCalled: () => boolean } {
  let called = false;
  const next: MiddlewareNext = () => {
    called = true;
    return Promise.resolve(
      new Response('<html>page</html>', {
        status: 200,
        headers: { 'Content-Type': 'text/html' },
      }),
    );
  };
  return { next, wasCalled: () => called };
}

async function run(
  options: Parameters<typeof makeContext>[0],
): Promise<{ response: Response; reachedRoute: boolean }> {
  const { next, wasCalled } = makeNext();
  const result = await onRequest(makeContext(options), next);
  if (!(result instanceof Response)) {
    throw new Error('middleware returned no Response');
  }
  return { response: result, reachedRoute: wasCalled() };
}

describe('crawler deny list', () => {
  const deniedUserAgents: readonly { bot: string; userAgent: string }[] = [
    {
      bot: 'GPTBot',
      userAgent:
        'Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko); compatible; GPTBot/1.1; +https://openai.com/gptbot',
    },
    {
      bot: 'ClaudeBot',
      userAgent:
        'Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; ClaudeBot/1.0; +claudebot@anthropic.com)',
    },
    { bot: 'CCBot', userAgent: 'CCBot/2.0 (https://commoncrawl.org/faq/)' },
    { bot: 'Google-Extended', userAgent: 'Google-Extended' },
    {
      bot: 'Bytespider',
      userAgent:
        'Mozilla/5.0 (Linux; Android 5.0) AppleWebKit/537.36 (KHTML, like Gecko) Mobile Safari/537.36 (compatible; Bytespider; spider-feedback@bytedance.com)',
    },
    {
      bot: 'PerplexityBot',
      userAgent:
        'Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; PerplexityBot/1.0; +https://perplexity.ai/perplexitybot)',
    },
    {
      bot: 'Amazonbot',
      userAgent:
        'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_10_1) AppleWebKit/600.2.5 (KHTML, like Gecko) Version/8.0.2 Safari/600.2.5 (Amazonbot/0.1; +https://developer.amazon.com/support/amazonbot)',
    },
    {
      bot: 'meta-externalagent',
      userAgent:
        'meta-externalagent/1.1 (+https://developers.facebook.com/docs/sharing/webmasters/crawler)',
    },
    {
      bot: 'ia_archiver',
      userAgent: 'ia_archiver (+http://www.alexa.com/site/help/webmasters; crawler@alexa.com)',
    },
    {
      bot: 'archive.org_bot',
      userAgent:
        'Mozilla/5.0 (compatible; archive.org_bot +http://archive.org/details/archive.org_bot)',
    },
    {
      bot: 'Googlebot',
      userAgent: 'Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)',
    },
    {
      bot: 'Bingbot',
      userAgent: 'Mozilla/5.0 (compatible; bingbot/2.0; +http://www.bing.com/bingbot.htm)',
    },
    {
      bot: 'DuckDuckBot',
      userAgent: 'DuckDuckBot/1.0; (+http://duckduckgo.com/duckduckbot.html)',
    },
    {
      bot: 'Baiduspider',
      userAgent:
        'Mozilla/5.0 (compatible; Baiduspider/2.0; +http://www.baidu.com/search/spider.html)',
    },
    {
      bot: 'YandexBot',
      userAgent: 'Mozilla/5.0 (compatible; YandexBot/3.0; +http://yandex.com/bots)',
    },
    {
      bot: 'Applebot',
      userAgent:
        'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko; compatible; Applebot/0.1; +http://www.apple.com/go/applebot)',
    },
  ];

  it('lists every mandated bot signature', () => {
    for (const { bot } of deniedUserAgents) {
      expect(CRAWLER_SIGNATURES).toContain(bot);
    }
  });

  it.each(deniedUserAgents)('denies $bot with 403 before any route runs', async (entry) => {
    const { response, reachedRoute } = await run({ userAgent: entry.userAgent });
    expect(response.status).toBe(403);
    expect(reachedRoute).toBe(false);
  });

  it('stamps X-Robots-Tag on the 403 as well', async () => {
    const { response } = await run({ userAgent: 'GPTBot/1.1' });
    expect(response.status).toBe(403);
    expect(response.headers.get('x-robots-tag')).toBe(X_ROBOTS_TAG_VALUE);
  });

  const allowedUserAgents: readonly { client: string; userAgent: string }[] = [
    {
      client: 'Chrome on Windows',
      userAgent:
        'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/127.0.0.0 Safari/537.36',
    },
    {
      client: 'Firefox on Linux',
      userAgent: 'Mozilla/5.0 (X11; Linux x86_64; rv:129.0) Gecko/20100101 Firefox/129.0',
    },
    {
      // AppleWebKit must not be caught by the Applebot signature.
      client: 'Safari on iPhone',
      userAgent:
        'Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1',
    },
    { client: 'curl', userAgent: 'curl/8.9.0' },
  ];

  it.each(allowedUserAgents)('lets $client through to the route', async (entry) => {
    const { response, reachedRoute } = await run({ userAgent: entry.userAgent });
    expect(response.status).toBe(200);
    expect(reachedRoute).toBe(true);
  });

  it('lets a request without a User-Agent header through', async () => {
    const { response, reachedRoute } = await run({});
    expect(response.status).toBe(200);
    expect(reachedRoute).toBe(true);
  });
});

describe('matchesCrawlerSignature', () => {
  it('is case-insensitive', () => {
    expect(matchesCrawlerSignature('gptbot/1.1')).toBe(true);
    expect(matchesCrawlerSignature('GPTBOT/1.1')).toBe(true);
  });

  it('does not match a missing or empty User-Agent', () => {
    expect(matchesCrawlerSignature(null)).toBe(false);
    expect(matchesCrawlerSignature('')).toBe(false);
  });

  it('does not match an ordinary browser User-Agent', () => {
    expect(
      matchesCrawlerSignature(
        'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/127.0.0.0 Safari/537.36',
      ),
    ).toBe(false);
  });
});

describe('robots.txt', () => {
  it('serves the disallow-all body from the middleware on GET', async () => {
    const { response, reachedRoute } = await run({ path: '/robots.txt' });
    expect(response.status).toBe(200);
    expect(await response.text()).toBe(ROBOTS_TXT_BODY);
    expect(response.headers.get('content-type')).toBe('text/plain; charset=utf-8');
    expect(reachedRoute).toBe(false);
  });

  it('disallows everything for every agent', () => {
    expect(ROBOTS_TXT_BODY).toBe('User-agent: *\nDisallow: /\n');
  });

  it('stamps X-Robots-Tag on robots.txt too', async () => {
    const { response } = await run({ path: '/robots.txt' });
    expect(response.headers.get('x-robots-tag')).toBe(X_ROBOTS_TAG_VALUE);
  });

  it('still denies a listed crawler asking for robots.txt', async () => {
    const { response } = await run({ path: '/robots.txt', userAgent: 'CCBot/2.0' });
    expect(response.status).toBe(403);
  });

  it('does not intercept non-GET requests for the path', async () => {
    const { reachedRoute } = await run({ path: '/robots.txt', method: 'POST' });
    expect(reachedRoute).toBe(true);
  });
});

describe('X-Robots-Tag advisory header', () => {
  it('is stamped on an ordinary page response', async () => {
    const { response, reachedRoute } = await run({ path: '/' });
    expect(reachedRoute).toBe(true);
    expect(response.headers.get('x-robots-tag')).toBe('noindex, nofollow');
    expect(await response.text()).toBe('<html>page</html>');
  });
});

describe('Vary: User-Agent cache safety', () => {
  it('is stamped on an ordinary page response', async () => {
    const { response } = await run({ path: '/' });
    expect(response.headers.get('vary')).toBe('User-Agent');
  });

  it('is stamped on the 403 so caches never reuse it for browsers', async () => {
    const { response } = await run({ userAgent: 'GPTBot/1.1' });
    expect(response.status).toBe(403);
    expect(response.headers.get('vary')).toBe('User-Agent');
  });

  it('is stamped on robots.txt', async () => {
    const { response } = await run({ path: '/robots.txt' });
    expect(response.headers.get('vary')).toBe('User-Agent');
  });

  it('merges with an existing Vary value instead of clobbering it', async () => {
    const next: MiddlewareNext = () =>
      Promise.resolve(new Response('page', { headers: { Vary: 'Accept-Language' } }));
    const result = await onRequest(makeContext({}), next);
    expect(result).toBeInstanceOf(Response);
    if (result instanceof Response) {
      expect(result.headers.get('vary')).toBe('Accept-Language, User-Agent');
    }
  });

  it('does not duplicate an already-present User-Agent field', async () => {
    const next: MiddlewareNext = () =>
      Promise.resolve(new Response('page', { headers: { Vary: 'user-agent' } }));
    const result = await onRequest(makeContext({}), next);
    expect(result).toBeInstanceOf(Response);
    if (result instanceof Response) {
      expect(result.headers.get('vary')).toBe('user-agent');
    }
  });

  it('leaves a wildcard Vary alone', async () => {
    const next: MiddlewareNext = () =>
      Promise.resolve(new Response('page', { headers: { Vary: '*' } }));
    const result = await onRequest(makeContext({}), next);
    expect(result).toBeInstanceOf(Response);
    if (result instanceof Response) {
      expect(result.headers.get('vary')).toBe('*');
    }
  });
});

describe('usage rollup logging', () => {
  // The counter is a per-process singleton, so this suite works with
  // whatever the earlier tests already recorded: it only asserts on the
  // final rollup and on the buckets its own requests must have produced.
  it('emits a usage_rollup once the interval elapses, holding only aggregates', async () => {
    vi.useFakeTimers();
    const log = vi.spyOn(console, 'log').mockImplementation(() => undefined);
    try {
      vi.setSystemTime(new Date('2026-08-16T10:00:00Z'));
      await run({ path: '/el/munich+greece' });
      await run({ path: '/el/munich+greece' });

      vi.setSystemTime(new Date('2026-08-16T10:06:00Z'));
      await run({ path: '/el/munich+greece' });

      const lastLine = log.mock.calls.at(-1)?.[0] as string;
      expect(lastLine).toBeDefined();
      const rollup = JSON.parse(lastLine) as {
        event: string;
        window_ended_at: string;
        counts: Record<string, unknown>[];
      };
      expect(rollup.event).toBe('usage_rollup');
      expect(rollup.window_ended_at).toBe('2026-08-16T10:06:00.000Z');

      const mine = rollup.counts.find(
        (count) => count['route'] === 'front' && count['lang'] === 'el' && count['status'] === 200,
      );
      expect(mine).toBeDefined();
      expect(mine?.['day']).toBe('2026-08-16');
      // The whole point: nothing beyond the five aggregate dimensions —
      // no IP, no User-Agent, no identifier of any kind.
      for (const count of rollup.counts) {
        expect(Object.keys(count).sort()).toEqual(['count', 'day', 'lang', 'route', 'status']);
      }
    } finally {
      log.mockRestore();
      vi.useRealTimers();
    }
  });
});
