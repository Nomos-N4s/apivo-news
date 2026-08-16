// Aggregate usage counting (issue #91) — the deliberate opposite of an
// analytics pixel. The product decision on #37 is that epiloYES measures
// itself without measuring its readers, so this module counts requests
// into buckets that cannot identify anyone even in combination: the
// Europe/Berlin day, the reading language, a coarse route class, and the
// response status. No IP, no User-Agent, no cookie, no session, no
// fingerprint — those values never even reach this file's API.
//
// The counter lives in server memory and is flushed periodically as one
// `usage_rollup` structured log line, so the deployment's ordinary log
// pipeline is the whole storage story.

import { isReadingLanguage } from './reader/axes';

/** The coarse shape of a request — never the concrete URL. */
export type RouteClass =
  | 'front'
  | 'article'
  | 'setup'
  | 'register'
  | 'go'
  | 'editor'
  | 'robots'
  | 'other';

/**
 * Classifies a pathname into its route class, or null for paths that must
 * not be counted at all (static assets). Classification is shape-based:
 * an article URL for an id that turns out to be a 404 still classifies as
 * 'article' — the status bucket carries the difference.
 */
export function classifyRoute(pathname: string): RouteClass | null {
  if (pathname === '/robots.txt') {
    return 'robots';
  }
  // The setup form's destination lives at the root — GET /go — because it
  // receives the language as form data rather than as a path segment.
  if (pathname === '/go') {
    return 'go';
  }
  const segments = pathname.split('/').filter((segment) => segment !== '');
  const last = segments.at(-1);
  if (last !== undefined && last.includes('.')) {
    return null;
  }
  const [langSegment, second, third] = segments;
  if (langSegment === undefined || !isReadingLanguage(langSegment)) {
    return 'other';
  }
  if (second === undefined) {
    return 'other';
  }
  if (second === 'setup') {
    return 'setup';
  }
  if (second === 'register') {
    return 'register';
  }
  if (second === 'go') {
    return 'go';
  }
  if (second === 'editor') {
    return 'editor';
  }
  // Anything else in the second slot is a place list.
  if (segments.length === 2) {
    return 'front';
  }
  if (third === 'a' && segments.length === 4) {
    return 'article';
  }
  return 'other';
}

/** The reading language a pathname claims, or 'none' outside the axes. */
export function pathLang(pathname: string): string {
  const first = pathname.split('/').find((segment) => segment !== '');
  return first !== undefined && isReadingLanguage(first) ? first : 'none';
}

// Day boundaries follow the newsroom clock (Europe/Berlin, like every
// timestamp the reader sees), not UTC — a rollup day should mean the same
// thing as the masthead date.
const dayFormat = new Intl.DateTimeFormat('en-CA', {
  timeZone: 'Europe/Berlin',
  year: 'numeric',
  month: '2-digit',
  day: '2-digit',
});

/**
 * The Europe/Berlin calendar day a moment falls on, as YYYY-MM-DD —
 * assembled from formatToParts rather than format(): only the time-zone
 * arithmetic is Intl's job here, because a locale's separator/order
 * conventions are CLDR data, not a spec guarantee, and the rollup day is
 * a machine key that downstream consumers sort and parse.
 */
export function usageDay(now: Date): string {
  const parts = new Map(
    dayFormat.formatToParts(now).map((part) => [part.type, part.value]),
  );
  const year = parts.get('year');
  const month = parts.get('month');
  const day = parts.get('day');
  if (year === undefined || month === undefined || day === undefined) {
    throw new Error('Intl.DateTimeFormat returned no year/month/day parts');
  }
  return `${year}-${month}-${day}`;
}

export interface UsageCount {
  readonly day: string;
  readonly lang: string;
  readonly route: RouteClass;
  readonly status: number;
  readonly count: number;
}

export interface UsageRollup {
  readonly event: 'usage_rollup';
  readonly window_started_at: string;
  readonly window_ended_at: string;
  /**
   * Requests that arrived after the key cap was reached and were counted
   * only here. Reported so a truncated window can never silently read as
   * a complete one.
   */
  readonly dropped_events: number;
  readonly counts: readonly UsageCount[];
}

export interface UsageCounter {
  /** Counts one served request. Asset paths are ignored entirely. */
  record(now: Date, pathname: string, status: number): void;
  /**
   * Emits a rollup through `sink` and resets, once per interval. The first
   * call only opens the window; an interval with nothing recorded advances
   * the window silently. Returns whether a rollup was emitted.
   */
  flushIfDue(now: Date, sink: (rollup: UsageRollup) => void): boolean;
  /** Unconditional flush (shutdown, tests). No-op while empty. */
  flush(now: Date, sink: (rollup: UsageRollup) => void): boolean;
}

export interface UsageCounterOptions {
  /** Milliseconds between rollups. Default: five minutes. */
  readonly flushIntervalMs?: number;
  /**
   * Upper bound on distinct buckets held in memory. The dimensions are
   * low-cardinality by construction, so hitting this means something is
   * malformed; overflow lands in dropped_events rather than growing.
   */
  readonly maxKeys?: number;
}

export function createUsageCounter(options: UsageCounterOptions = {}): UsageCounter {
  const flushIntervalMs = options.flushIntervalMs ?? 5 * 60_000;
  const maxKeys = options.maxKeys ?? 512;

  const buckets = new Map<string, UsageCount & { count: number }>();
  let droppedEvents = 0;
  let windowStartedAt: Date | undefined;

  const flush = (now: Date, sink: (rollup: UsageRollup) => void): boolean => {
    if (buckets.size === 0 && droppedEvents === 0) {
      windowStartedAt = now;
      return false;
    }
    const counts = [...buckets.entries()]
      .sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))
      .map(([, bucket]) => ({ ...bucket }));
    sink({
      event: 'usage_rollup',
      window_started_at: (windowStartedAt ?? now).toISOString(),
      window_ended_at: now.toISOString(),
      dropped_events: droppedEvents,
      counts,
    });
    buckets.clear();
    droppedEvents = 0;
    windowStartedAt = now;
    return true;
  };

  return {
    record(now, pathname, status) {
      const route = classifyRoute(pathname);
      if (route === null) {
        return;
      }
      const day = usageDay(now);
      const lang = pathLang(pathname);
      const key = `${day}|${lang}|${route}|${String(status)}`;
      const bucket = buckets.get(key);
      if (bucket !== undefined) {
        bucket.count += 1;
        return;
      }
      if (buckets.size >= maxKeys) {
        droppedEvents += 1;
        return;
      }
      buckets.set(key, { day, lang, route, status, count: 1 });
    },
    flushIfDue(now, sink) {
      if (windowStartedAt === undefined) {
        windowStartedAt = now;
        return false;
      }
      if (now.getTime() - windowStartedAt.getTime() < flushIntervalMs) {
        return false;
      }
      return flush(now, sink);
    },
    flush,
  };
}
