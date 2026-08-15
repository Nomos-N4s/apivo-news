import { describe, expect, it, vi } from 'vitest';

import {
  classifyRoute,
  createUsageCounter,
  pathLang,
  usageDay,
  type UsageRollup,
} from './usage';

describe('classifyRoute', () => {
  it('classifies every screen by shape', () => {
    expect(classifyRoute('/el/munich+greece')).toBe('front');
    expect(classifyRoute('/de/munich')).toBe('front');
    expect(classifyRoute('/el/munich+greece/a/0d9f4a12')).toBe('article');
    expect(classifyRoute('/el/setup')).toBe('setup');
    expect(classifyRoute('/de/register')).toBe('register');
    expect(classifyRoute('/el/go')).toBe('go');
    expect(classifyRoute('/el/editor')).toBe('editor');
    expect(classifyRoute('/de/editor/audit')).toBe('editor');
    expect(classifyRoute('/robots.txt')).toBe('robots');
  });

  it('classifies by shape even when the id will 404 — status carries that', () => {
    expect(classifyRoute('/el/munich/a/no-such-article')).toBe('article');
  });

  it('sends unknown shapes to other, never to a page class', () => {
    expect(classifyRoute('/')).toBe('other');
    expect(classifyRoute('/el')).toBe('other');
    expect(classifyRoute('/fr/paris')).toBe('other');
    expect(classifyRoute('/el/munich/extra/deep/path')).toBe('other');
  });

  it('refuses to count assets at all', () => {
    expect(classifyRoute('/fonts/archivo-latin.woff2')).toBeNull();
    expect(classifyRoute('/favicon.svg')).toBeNull();
  });
});

describe('pathLang', () => {
  it('reads the language axis and nothing else', () => {
    expect(pathLang('/el/munich+greece')).toBe('el');
    expect(pathLang('/de/register')).toBe('de');
    expect(pathLang('/fr/paris')).toBe('none');
    expect(pathLang('/')).toBe('none');
  });
});

describe('usageDay', () => {
  it('uses the newsroom day, not the UTC day', () => {
    // 23:30 UTC on the 15th is already 01:30 CEST on the 16th in Berlin.
    expect(usageDay(new Date('2026-08-15T23:30:00Z'))).toBe('2026-08-16');
    expect(usageDay(new Date('2026-08-15T12:00:00Z'))).toBe('2026-08-15');
  });
});

const at = (iso: string): Date => new Date(iso);

describe('createUsageCounter', () => {
  it('aggregates identical buckets and separates different ones', () => {
    const counter = createUsageCounter();
    counter.record(at('2026-08-15T10:00:00Z'), '/el/munich+greece', 200);
    counter.record(at('2026-08-15T10:01:00Z'), '/el/munich+greece', 200);
    counter.record(at('2026-08-15T10:02:00Z'), '/de/munich+greece', 200);
    counter.record(at('2026-08-15T10:03:00Z'), '/el/munich/a/x1', 404);

    const sink = vi.fn<(rollup: UsageRollup) => void>();
    expect(counter.flush(at('2026-08-15T10:05:00Z'), sink)).toBe(true);
    const rollup = sink.mock.calls[0]?.[0];
    expect(rollup?.event).toBe('usage_rollup');
    expect(rollup?.counts).toEqual([
      { day: '2026-08-15', lang: 'de', route: 'front', status: 200, count: 1 },
      { day: '2026-08-15', lang: 'el', route: 'article', status: 404, count: 1 },
      { day: '2026-08-15', lang: 'el', route: 'front', status: 200, count: 2 },
    ]);
    expect(rollup?.dropped_events).toBe(0);
  });

  it('holds nothing that could identify a reader', () => {
    const counter = createUsageCounter();
    counter.record(at('2026-08-15T10:00:00Z'), '/el/munich+greece', 200);
    const sink = vi.fn<(rollup: UsageRollup) => void>();
    counter.flush(at('2026-08-15T10:05:00Z'), sink);
    const keys = Object.keys(sink.mock.calls[0]?.[0]?.counts[0] ?? {});
    expect(keys.sort()).toEqual(['count', 'day', 'lang', 'route', 'status']);
  });

  it('ignores asset paths entirely', () => {
    const counter = createUsageCounter();
    counter.record(at('2026-08-15T10:00:00Z'), '/fonts/archivo-latin.woff2', 200);
    const sink = vi.fn<(rollup: UsageRollup) => void>();
    expect(counter.flush(at('2026-08-15T10:05:00Z'), sink)).toBe(false);
    expect(sink).not.toHaveBeenCalled();
  });

  it('reports overflow as dropped_events instead of losing it silently', () => {
    const counter = createUsageCounter({ maxKeys: 1 });
    counter.record(at('2026-08-15T10:00:00Z'), '/el/munich', 200);
    counter.record(at('2026-08-15T10:00:01Z'), '/de/munich', 200);
    counter.record(at('2026-08-15T10:00:02Z'), '/el/munich', 200);

    const sink = vi.fn<(rollup: UsageRollup) => void>();
    counter.flush(at('2026-08-15T10:05:00Z'), sink);
    const rollup = sink.mock.calls[0]?.[0];
    expect(rollup?.counts).toEqual([
      { day: '2026-08-15', lang: 'el', route: 'front', status: 200, count: 2 },
    ]);
    expect(rollup?.dropped_events).toBe(1);
  });

  it('flushIfDue opens the window first, then emits once per interval', () => {
    const counter = createUsageCounter({ flushIntervalMs: 60_000 });
    const sink = vi.fn<(rollup: UsageRollup) => void>();

    expect(counter.flushIfDue(at('2026-08-15T10:00:00Z'), sink)).toBe(false);
    counter.record(at('2026-08-15T10:00:10Z'), '/el/munich', 200);
    expect(counter.flushIfDue(at('2026-08-15T10:00:30Z'), sink)).toBe(false);
    expect(counter.flushIfDue(at('2026-08-15T10:01:05Z'), sink)).toBe(true);
    expect(sink).toHaveBeenCalledTimes(1);
    expect(sink.mock.calls[0]?.[0]?.window_started_at).toBe('2026-08-15T10:00:00.000Z');
    expect(sink.mock.calls[0]?.[0]?.window_ended_at).toBe('2026-08-15T10:01:05.000Z');

    // The next window starts where the flush happened and stays silent
    // while empty.
    expect(counter.flushIfDue(at('2026-08-15T10:02:10Z'), sink)).toBe(false);
    expect(sink).toHaveBeenCalledTimes(1);
  });

  it('flush on an empty counter is a no-op', () => {
    const counter = createUsageCounter();
    const sink = vi.fn<(rollup: UsageRollup) => void>();
    expect(counter.flush(at('2026-08-15T10:00:00Z'), sink)).toBe(false);
    expect(sink).not.toHaveBeenCalled();
  });
});
