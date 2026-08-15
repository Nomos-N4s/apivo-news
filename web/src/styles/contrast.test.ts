import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';

// WCAG contrast floors, asserted against the actual token values in
// modernist.css. The design system is vendored, so nothing upstream
// protects these ratios: a well-meaning "brighten the palette" edit would
// otherwise regress issue #89 without failing anything. The test parses
// the stylesheet rather than repeating the hex values, so it follows the
// tokens wherever they go. (Read with fs: vitest's CSS pipeline turns even
// a `?raw` stylesheet import into an empty module.)

const css = readFileSync(fileURLToPath(new URL('./modernist.css', import.meta.url)), 'utf8');

function token(name: string): string {
  const match = css.match(new RegExp(`${name}:\\s*([^;]+);`));
  if (match?.[1] === undefined) {
    throw new Error(`modernist.css no longer defines ${name}`);
  }
  return match[1].trim();
}

type Rgb = readonly [number, number, number];

function hex(value: string): Rgb {
  const match = value.match(/^#([0-9a-f]{6})$/i);
  if (match?.[1] === undefined) {
    throw new Error(`expected a 6-digit hex color, got "${value}"`);
  }
  const digits = match[1];
  return [0, 2, 4].map((i) => parseInt(digits.slice(i, i + 2), 16)) as unknown as Rgb;
}

/** An ink-over-transparent color-mix() composited onto a backdrop. */
function mixOver(value: string, backdrop: Rgb): Rgb {
  const match = value.match(/^color-mix\(in srgb, (#[0-9a-f]{6}) (\d+)%, transparent\)$/i);
  if (match?.[1] === undefined || match[2] === undefined) {
    throw new Error(`expected an ink/transparent color-mix, got "${value}"`);
  }
  const ink = hex(match[1]);
  const pct = Number(match[2]);
  return [0, 1, 2].map(
    (i) => Math.round((ink[i]! * pct) / 100 + (backdrop[i]! * (100 - pct)) / 100),
  ) as unknown as Rgb;
}

function luminance(rgb: Rgb): number {
  const channel = (value: number): number => {
    const c = value / 255;
    return c <= 0.04045 ? c / 12.92 : Math.pow((c + 0.055) / 1.055, 2.4);
  };
  return 0.2126 * channel(rgb[0]) + 0.7152 * channel(rgb[1]) + 0.0722 * channel(rgb[2]);
}

function ratio(a: Rgb, b: Rgb): number {
  const [lighter, darker] = [luminance(a), luminance(b)].sort((x, y) => y - x) as [number, number];
  return (lighter + 0.05) / (darker + 0.05);
}

const bg = hex(token('--color-bg'));
const surface = hex(token('--color-surface'));
const text = hex(token('--color-text'));
const accent = hex(token('--color-accent'));
const accent700 = hex(token('--color-accent-700'));
const accent800 = hex(token('--color-accent-800'));
const accent900 = hex(token('--color-accent-900'));

describe('text contrast (WCAG 1.4.3, 4.5:1)', () => {
  it('body text passes on both backgrounds', () => {
    expect(ratio(text, bg)).toBeGreaterThanOrEqual(4.5);
    expect(ratio(text, surface)).toBeGreaterThanOrEqual(4.5);
  });

  it('accent-700 — the small-accent-text step — passes on both backgrounds', () => {
    expect(ratio(accent700, bg)).toBeGreaterThanOrEqual(4.5);
    expect(ratio(accent700, surface)).toBeGreaterThanOrEqual(4.5);
  });

  it('light text passes on the accent fill and its hover/active steps', () => {
    expect(ratio(bg, accent700)).toBeGreaterThanOrEqual(4.5);
    expect(ratio(bg, accent800)).toBeGreaterThanOrEqual(4.5);
    expect(ratio(bg, accent900)).toBeGreaterThanOrEqual(4.5);
  });

  it('both muted ink tokens pass on both backgrounds', () => {
    for (const name of ['--ink-muted', '--ink-soft']) {
      expect(ratio(mixOver(token(name), bg), bg)).toBeGreaterThanOrEqual(4.5);
      expect(ratio(mixOver(token(name), surface), surface)).toBeGreaterThanOrEqual(4.5);
    }
  });
});

describe('composed states — opacity over fills', () => {
  // Two individually passing colors can render a failing pair when a
  // descendant carries opacity: the text composites over its ancestor's
  // fill. This is why de-emphasis in the pages uses the explicit ink
  // tokens and never opacity — these assertions pin the arithmetic.
  const over = (top: Rgb, alpha: number, backdrop: Rgb): Rgb =>
    [0, 1, 2].map((i) =>
      Math.round(top[i]! * alpha + backdrop[i]! * (1 - alpha)),
    ) as unknown as Rgb;

  it('light ink dimmed to 75% over accent-700 fails; the full ink passes', () => {
    expect(ratio(over(bg, 0.75, accent700), accent700)).toBeLessThan(4.5);
    expect(ratio(bg, accent700)).toBeGreaterThanOrEqual(4.5);
  });

  it('body ink halved over the page fails; --ink-muted passes', () => {
    expect(ratio(over(text, 0.5, bg), bg)).toBeLessThan(4.5);
    expect(ratio(mixOver(token('--ink-muted'), bg), bg)).toBeGreaterThanOrEqual(4.5);
  });
});

describe('non-text contrast (WCAG 1.4.11, 3:1)', () => {
  it('control boundaries pass on both backgrounds', () => {
    expect(ratio(mixOver(token('--border-strong'), bg), bg)).toBeGreaterThanOrEqual(3);
    expect(ratio(mixOver(token('--border-strong'), surface), surface)).toBeGreaterThanOrEqual(3);
  });

  it('accent-500 state marks (radio dots, checkbox fills, focus borders) pass', () => {
    expect(ratio(accent, bg)).toBeGreaterThanOrEqual(3);
    expect(ratio(accent, surface)).toBeGreaterThanOrEqual(3);
  });

  it('the consent-switch off track and its knob pass', () => {
    const track = hex(token('--color-neutral-600'));
    expect(ratio(track, surface)).toBeGreaterThanOrEqual(3);
    expect(ratio(bg, track)).toBeGreaterThanOrEqual(3);
  });

  it('the spend-meter fill reads against its track', () => {
    expect(ratio(accent700, hex(token('--color-neutral-300')))).toBeGreaterThanOrEqual(3);
  });
});
