import { describe, expect, it } from 'vitest';

import {
  formatItemDate,
  formatMastheadDate,
  formatRecency,
  safeSourceUrl,
  sourceHost,
} from './format';

// 08:12 UTC = 10:12 in Europe/Berlin (CEST) — the reader locale's zone.
const PUBLISHED = new Date('2026-08-14T08:12:00Z');

describe('formatMastheadDate', () => {
  it('writes the full day in Greek', () => {
    expect(formatMastheadDate('el', PUBLISHED)).toBe('Παρασκευή 14 Αυγούστου 2026');
  });

  it('writes the full day in German', () => {
    expect(formatMastheadDate('de', PUBLISHED)).toBe('Freitag, 14. August 2026');
  });

  it('renders in the reader zone, not the server zone', () => {
    // 23:30 UTC on the 14th is already the 15th in Munich.
    const lateEvening = new Date('2026-08-14T23:30:00Z');
    expect(formatMastheadDate('de', lateEvening)).toContain('15.');
  });
});

describe('formatItemDate', () => {
  it('writes the date without the weekday', () => {
    expect(formatItemDate('el', PUBLISHED)).toBe('14 Αυγούστου 2026');
    expect(formatItemDate('de', PUBLISHED)).toBe('14. August 2026');
  });
});

describe('formatRecency', () => {
  it('speaks in minutes inside the first hour', () => {
    const now = new Date('2026-08-14T08:47:00Z');
    expect(formatRecency('el', PUBLISHED, now)).toBe('πριν από 35 λεπτά');
    expect(formatRecency('de', PUBLISHED, now)).toBe('vor 35 Minuten');
  });

  it('speaks in hours inside the first day', () => {
    const now = new Date('2026-08-14T10:12:00Z');
    expect(formatRecency('el', PUBLISHED, now)).toBe('πριν από 2 ώρες');
    expect(formatRecency('de', PUBLISHED, now)).toBe('vor 2 Stunden');
  });

  it('falls back to the plain date beyond a day', () => {
    const now = new Date('2026-08-16T09:00:00Z');
    expect(formatRecency('de', PUBLISHED, now)).toBe('14. August 2026');
  });

  it('clamps a future timestamp to now instead of announcing time travel', () => {
    const now = new Date('2026-08-14T08:00:00Z');
    const atNow = formatRecency('de', now, now);
    expect(formatRecency('de', PUBLISHED, now)).toBe(atNow);
  });
});

describe('sourceHost', () => {
  it('labels the link by its host, dropping www', () => {
    expect(sourceHost('https://www.tagblatt-muenchen.example/a/b')).toBe(
      'tagblatt-muenchen.example',
    );
    expect(sourceHost('https://isarkurier.example/x')).toBe('isarkurier.example');
  });

  it('returns a malformed value as-is rather than failing the page', () => {
    expect(sourceHost('not a url')).toBe('not a url');
  });

  it('never shows a blank Source: hostless schemes display their raw value', () => {
    expect(sourceHost('javascript:alert(1)')).toBe('javascript:alert(1)');
    expect(sourceHost('mailto:x@example.com')).toBe('mailto:x@example.com');
  });
});

describe('safeSourceUrl', () => {
  it('passes http and https through', () => {
    expect(safeSourceUrl('https://isarkurier.example/x')).toBe(
      'https://isarkurier.example/x',
    );
    expect(safeSourceUrl('http://isarkurier.example/x')).toBe('http://isarkurier.example/x');
  });

  it('refuses every other scheme — feed data must never become a clickable script', () => {
    expect(safeSourceUrl('javascript:alert(1)')).toBeNull();
    expect(safeSourceUrl('data:text/html,<script>alert(1)</script>')).toBeNull();
    expect(safeSourceUrl('vbscript:msgbox(1)')).toBeNull();
    expect(safeSourceUrl('file:///etc/passwd')).toBeNull();
  });

  it('refuses what does not parse at all', () => {
    expect(safeSourceUrl('not a url')).toBeNull();
    expect(safeSourceUrl('')).toBeNull();
  });
});
