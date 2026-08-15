import type { ReadingLanguage } from './axes';

/**
 * Date and source-link presentation for the reader pages.
 *
 * Times render in the alpha reader locale's zone (Munich — Europe/Berlin):
 * the spec scopes the alpha to Munich as reader locale, and the server's
 * own zone (UTC in containers) must never leak into the page.
 */
const READER_TIME_ZONE = 'Europe/Berlin';

const MS_PER_MINUTE = 60_000;
const MS_PER_HOUR = 3_600_000;
const HOURS_PER_DAY = 24;

/** The masthead date — e.g. el "Πέμπτη 14 Αυγούστου 2026", de "Donnerstag, 14. August 2026". */
export function formatMastheadDate(lang: ReadingLanguage, date: Date): string {
  return new Intl.DateTimeFormat(lang, {
    weekday: 'long',
    day: 'numeric',
    month: 'long',
    year: 'numeric',
    timeZone: READER_TIME_ZONE,
  }).format(date);
}

/** A full date for item metadata — e.g. el "14 Αυγούστου 2026". */
export function formatItemDate(lang: ReadingLanguage, date: Date): string {
  return new Intl.DateTimeFormat(lang, {
    day: 'numeric',
    month: 'long',
    year: 'numeric',
    timeZone: READER_TIME_ZONE,
  }).format(date);
}

/**
 * Recency for item meta rows: relative within the last day ("πριν από 2
 * ώρες", "vor 2 Stunden"), the plain date beyond it. A published time in
 * the future — clock skew between machines — clamps to "now" rather than
 * announcing time travel.
 */
export function formatRecency(lang: ReadingLanguage, published: Date, now: Date): string {
  const elapsedMs = Math.max(0, now.getTime() - published.getTime());
  const relative = new Intl.RelativeTimeFormat(lang, { numeric: 'auto' });
  if (elapsedMs < MS_PER_HOUR) {
    return relative.format(-Math.floor(elapsedMs / MS_PER_MINUTE), 'minute');
  }
  if (elapsedMs < HOURS_PER_DAY * MS_PER_HOUR) {
    return relative.format(-Math.floor(elapsedMs / MS_PER_HOUR), 'hour');
  }
  return formatItemDate(lang, published);
}

/**
 * The display host of an outbound source link — "sueddeutsche.de" for the
 * card's link text. The public payload carries no structured publisher
 * name (only the composed attribution prose), so the link is labelled by
 * where it honestly goes; the full attribution renders alongside.
 */
export function sourceHost(sourceUrl: string): string {
  try {
    return new URL(sourceUrl).hostname.replace(/^www\./, '');
  } catch {
    // A malformed source_url must not take the page down; the raw value
    // still names where the link goes.
    return sourceUrl;
  }
}

/**
 * A feed-provided URL vetted for use as a link target: only http(s) may
 * render as an href. Feed data is external input — a `javascript:` URL
 * must never reach the page as something clickable — so anything else
 * answers null and the caller renders plain text instead.
 */
export function safeSourceUrl(sourceUrl: string): string | null {
  try {
    const url = new URL(sourceUrl);
    return url.protocol === 'http:' || url.protocol === 'https:' ? url.href : null;
  } catch {
    return null;
  }
}
