/**
 * The brand a deployment runs under, read on the TypeScript side.
 *
 * This is the second reader of one file. The first is the Go package
 * internal/platform/brand, and neither of them describes the brand
 * schema: it is defined once, as Go types, and `brand.types.ts` is
 * generated from them — both the interfaces and the same schema again as
 * data, which is what `assertBrand` walks. A field added on one side and
 * forgotten on the other is a failing test in CI rather than a surface
 * that renders half a brand.
 *
 * ---------------------------------------------------------------------------
 * What this module deliberately does NOT do
 *
 * 1. IT DOES NOT READ FILES. Reading bytes is the one part that differs
 *    between a build step, a request and a test, so it belongs to the
 *    caller. Keeping `node:fs` out of here also keeps the module usable
 *    wherever a brand is already in hand.
 *
 * 2. IT DOES NOT RE-JUDGE THE BRAND. The rules about what a valid brand
 *    MEANS — that a support address sits on the brand's own domain, that
 *    a payout descriptor survives a card statement — live in the Go
 *    package, once. Restating them here is exactly the hand-duplication
 *    the generated schema exists to avoid. What this module checks is
 *    shape: that every field the schema declares is present, that none
 *    of them is the wrong type, and that the file carries nothing the
 *    schema has never heard of.
 *
 * The one judgement it does make is its own: a value that would break out
 * of the stylesheet it is about to be written into is refused, because
 * that is a decision about this module's output rather than about the
 * brand.
 */
import { brandRoot, brandSchema } from './brand.types';
import type { BrandFieldType, Brand } from './brand.types';

export type {
  Brand,
  BrandFieldType,
  BrandInterfaceSchema,
  Legal,
  Document,
  Domains,
  Support,
  Assets,
  Theme,
  Typography,
  Defaults,
  Payout,
} from './brand.types';
export { brandRoot, brandSchema } from './brand.types';

/**
 * The name a brand definition carries inside its directory — the same
 * constant as `brand.FileName` in Go, because it is the same file.
 */
export const BRAND_FILE_NAME = 'brand.json';

/**
 * A brand file that cannot be trusted to render a surface.
 *
 * It names every problem at once, like the Go loader does: filling in a
 * brand definition one error per run is a poor way to spend an
 * afternoon.
 */
export class BrandError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'BrandError';
  }
}

/**
 * Parse and check a brand definition.
 *
 * Throws `BrandError` rather than returning null: a caller with no brand
 * has nothing to render and no useful fallback, and a page that quietly
 * substituted a default would be a page silently serving the wrong
 * brand.
 */
export function parseBrand(json: string): Brand {
  let value: unknown;
  try {
    value = JSON.parse(json);
  } catch {
    throw new BrandError('brand: the definition is not valid JSON');
  }
  assertBrand(value);
  return value;
}

/**
 * Check a decoded value against the generated schema, in place.
 *
 * Every field the schema declares must be present and of the declared
 * type, and every key present must be one the schema declares — a
 * misspelled key is a value that silently does not apply, which is how a
 * surface keeps the previous brand's colour and nobody can say why.
 */
export function assertBrand(value: unknown): asserts value is Brand {
  const problems: string[] = [];
  checkInterface('', brandRoot, value, problems);
  if (problems.length > 0) {
    problems.sort();
    throw new BrandError(`brand: invalid configuration: ${problems.join('; ')}`);
  }
}

/**
 * The brand's design tokens as CSS custom properties.
 *
 * The names are the ones `styles/modernist.css` already reads —
 * `--color-*` for the palette, `--font-*` for the type — so a brand
 * fills the variables the stylesheet declares rather than introducing a
 * parallel vocabulary that every rule would then have to learn.
 *
 * Colour tokens come out in name order, so the same brand always renders
 * the same stylesheet and a diff of two builds is about the brand.
 */
export function brandCustomProperties(brand: Brand): Record<string, string> {
  const properties: Record<string, string> = {};
  // Sorted by code point rather than by locale: the same brand must
  // render the same stylesheet on every machine that builds it. Two keys
  // of one object are never equal, so there is no third case.
  const colours = Object.entries(brand.theme.colours).sort(([left], [right]) => (left < right ? -1 : 1));
  for (const [token, value] of colours) {
    properties[`--color-${token}`] = cssValue(`theme.colours.${token}`, value);
  }
  properties['--font-heading'] = cssValue('theme.typography.heading', brand.theme.typography.heading);
  properties['--font-heading-weight'] = String(brand.theme.typography.headingWeight);
  properties['--font-body'] = cssValue('theme.typography.body', brand.theme.typography.body);
  return properties;
}

/**
 * The brand's custom properties as a stylesheet, ready to be written
 * into a `<style>` element ahead of the design system.
 */
export function brandStyleSheet(brand: Brand, selector = ':root'): string {
  const declarations = Object.entries(brandCustomProperties(brand))
    .map(([name, value]) => `  ${name}: ${value};`)
    .join('\n');
  return `${selector} {\n${declarations}\n}\n`;
}

/**
 * A brand value on its way into a stylesheet.
 *
 * Anything that could end a declaration, end the rule or end the
 * `<style>` element is refused rather than escaped: a brand file is
 * authored, not submitted, so a character that has no business in a
 * colour or a font stack is a mistake worth stopping for.
 */
function cssValue(path: string, value: string): string {
  if (/[;{}<>\\]|[\r\n]/.test(value)) {
    throw new BrandError(`brand: ${path} contains a character that would break out of the stylesheet`);
  }
  return value;
}

/** Whether a decoded JSON value is an object with named fields. */
function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

/** `parent.child`, or just `child` at the root. */
function join(path: string, field: string): string {
  return path === '' ? field : `${path}.${field}`;
}

/** Check one value against the field type the schema declares for it. */
function checkValue(path: string, expected: BrandFieldType, value: unknown, problems: string[]): void {
  if (expected === 'string' || expected === 'number' || expected === 'boolean') {
    if (typeof value !== expected) {
      problems.push(`${path} is ${describe(value)}, want ${expected}`);
    } else if (expected === 'number' && !Number.isFinite(value)) {
      problems.push(`${path} is not a finite number`);
    }
    return;
  }
  if ('struct' in expected) {
    checkInterface(path, expected.struct, value, problems);
    return;
  }
  if ('list' in expected) {
    if (!Array.isArray(value)) {
      problems.push(`${path} is ${describe(value)}, want a list`);
      return;
    }
    value.forEach((item: unknown, index: number) => {
      checkValue(`${path}[${index}]`, expected.list, item, problems);
    });
    return;
  }
  if (!isRecord(value)) {
    problems.push(`${path} is ${describe(value)}, want an object`);
    return;
  }
  for (const [key, item] of Object.entries(value)) {
    checkValue(join(path, key), expected.map, item, problems);
  }
}

/** Check one object against the named interface the schema declares. */
function checkInterface(path: string, name: string, value: unknown, problems: string[]): void {
  const fields = brandSchema[name];
  // The generator only ever names interfaces it also emits, so this is
  // unreachable through the committed schema. It stays because the
  // generated file is still a file somebody could edit, and because a
  // schema that cannot describe itself must say so rather than pass
  // everything.
  if (fields === undefined) {
    problems.push(`${path === '' ? 'the brand' : path}: the schema declares no interface named ${name}`);
    return;
  }
  if (!isRecord(value)) {
    problems.push(`${path === '' ? 'the brand' : path} is ${describe(value)}, want an object`);
    return;
  }
  for (const [field, expected] of Object.entries(fields)) {
    if (!(field in value)) {
      problems.push(`${join(path, field)} is missing`);
      continue;
    }
    checkValue(join(path, field), expected, value[field], problems);
  }
  for (const key of Object.keys(value)) {
    if (!(key in fields)) {
      problems.push(`${join(path, key)} is not part of the brand schema`);
    }
  }
}

/** How a wrong value is named in a problem report. */
function describe(value: unknown): string {
  if (value === null) {
    return 'null';
  }
  if (Array.isArray(value)) {
    return 'a list';
  }
  if (typeof value === 'object') {
    return 'an object';
  }
  return `a ${typeof value}`;
}
