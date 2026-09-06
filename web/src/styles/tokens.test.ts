import { readdirSync, readFileSync, statSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { extname, join } from 'node:path';

import { describe, expect, it } from 'vitest';

/**
 * Every design token a stylesheet reads must be one the design system
 * defines (issue #519).
 *
 * A `var()` naming a custom property that does not exist, with no
 * fallback, makes the whole declaration **invalid at computed-value
 * time**. A non-inherited property then takes its initial value, so
 * `padding: var(--space-7) calc(…)` is not "28px, roughly" — it is `0`,
 * silently, with no console warning and nothing red anywhere.
 *
 * That is how the about page came to have no padding at all and the
 * fixture notice — the banner telling a reader the names on the page are
 * invented — came to have none either. Both referenced `--space-5` and
 * `--space-7`, which the scale has never carried: it runs 1, 2, 3, 4, 6,
 * 8, where the number is the multiplier of 4px, and 20px and 28px are
 * steps it deliberately omits.
 *
 * The precedent for asserting this here rather than trusting upstream is
 * `contrast.test.ts` in this directory: the design system is vendored, so
 * nothing outside the repository protects it, and what the repository
 * needs it asserts itself.
 */

const stylesDir = fileURLToPath(new URL('.', import.meta.url));
const srcDir = fileURLToPath(new URL('..', import.meta.url));

/** Custom properties `modernist.css` defines. */
const systemTokens = definitionsIn(readFileSync(join(stylesDir, 'modernist.css'), 'utf8'));

/** Every `--token:` declaration in a source text. */
function definitionsIn(source: string): Set<string> {
  return new Set(source.match(/--[a-zA-Z0-9-]+(?=\s*:)/g) ?? []);
}

/**
 * Every `var(--token)` reference that carries NO fallback.
 *
 * A reference written `var(--x, 8px)` is deliberate and safe — the
 * fallback is what makes it so — and is not reported.
 */
function referencesWithoutFallback(source: string): string[] {
  const found: string[] = [];
  for (const match of source.matchAll(/var\(\s*(--[a-zA-Z0-9-]+)\s*([,)])/g)) {
    const token = match[1];
    if (token !== undefined && match[2] === ')') {
      found.push(token);
    }
  }
  return found;
}

/** Every `.astro` and `.css` file under `src/`, recursively. */
function styledFiles(dir: string): string[] {
  const found: string[] = [];
  for (const entry of readdirSync(dir)) {
    const path = join(dir, entry);
    if (statSync(path).isDirectory()) {
      found.push(...styledFiles(path));
      continue;
    }
    if (extname(path) === '.astro' || extname(path) === '.css') {
      found.push(path);
    }
  }
  return found;
}

const files = styledFiles(srcDir);

describe('design tokens', () => {
  it('finds the files it is meant to be checking', () => {
    // A walk that silently matched nothing would pass every assertion
    // below while proving nothing at all.
    expect(files.length).toBeGreaterThan(10);
    expect(systemTokens.size).toBeGreaterThan(20);
  });

  it.each(files.map((path) => [path.slice(srcDir.length).replace(/\\/g, '/'), path]))(
    '%s references only tokens that exist',
    (_label, path) => {
      const source = readFileSync(path, 'utf8');
      // A component may define its own custom properties and read them
      // back; those are as real as the system's.
      const local = definitionsIn(source);
      const undefinedTokens = referencesWithoutFallback(source).filter(
        (token) => !systemTokens.has(token) && !local.has(token),
      );

      expect(undefinedTokens).toEqual([]);
    },
  );
});
