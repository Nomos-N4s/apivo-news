/**
 * Reading the brand a deployment names, which `index.ts` deliberately
 * does not do.
 *
 * `index.ts` says why in its own header: reading bytes is the one part
 * that differs between a build step, a request and a test, so it belongs
 * to the caller. This module is that caller, and it keeps the same
 * discipline one level up — it takes the environment as arguments rather
 * than importing `astro:env/server`, exactly as `createReaderApi` takes
 * `API_BASE_URL` and `APP_ENV` from the page that read them. A module
 * that reaches for the environment itself can only be tested by mutating
 * it.
 *
 * The fallback is the same shape as the reader's, for the same reason.
 * With no `BRAND_DIR` a development deployment renders the repository's
 * fixture brand and every surface that shows it says so; a deployed one
 * refuses. Printing "Zephyra Fixture Kooperativ AB, Göteborg" as a German
 * service's provider identification under TMG §5 is not a placeholder, it
 * is a false statement about who is legally responsible — and unlike an
 * invented publisher on a reader page, it is the exact statement the page
 * exists to make.
 */
import { readFileSync } from 'node:fs';
import { join } from 'node:path';

import { APP_ENV_DEV, APP_ENV_PROD, parseAppEnv } from '../app-env';
import { BRAND_FILE_NAME, BrandError, parseBrand } from './index';
import type { Brand } from './brand.types';
// The fixture brand's own bytes. Held as JSON rather than as a
// TypeScript literal so that `load.test.ts` can assert it is
// byte-identical to `internal/platform/brand/testdata/fixture/brand.json`.
// Two copies of one fixture that drift apart would make the rebrand proof
// pass here and fail there, which is the failure that proof exists to
// catch.
import fixtureBrand from './fixture.brand.json';

const fixtureBrandJson = JSON.stringify(fixtureBrand);

/**
 * Where the brand on hand came from.
 *
 * `deployment` is a brand file this deployment named. `fixture` is the
 * repository's own, and any surface rendering it must say so — it names
 * a company that does not exist.
 */
export type BrandSource = typeof BRAND_SOURCE_DEPLOYMENT | typeof BRAND_SOURCE_FIXTURE;

/** A brand file the deployment named through `BRAND_DIR`. */
export const BRAND_SOURCE_DEPLOYMENT = 'deployment';

/** The repository's fixture brand, standing in during development. */
export const BRAND_SOURCE_FIXTURE = 'fixture';

/** A brand, and whether it is this deployment's or the fixture. */
export interface LoadedBrand {
  readonly brand: Brand;
  readonly source: BrandSource;
}

/** What `loadBrand` needs from its caller. */
export interface LoadBrandOptions {
  /** `BRAND_DIR`, verbatim. */
  readonly brandDir?: string | undefined;
  /** `APP_ENV`, verbatim; an unreadable value is refused, never assumed. */
  readonly appEnv?: string | undefined;
  /** Seam for tests; defaults to reading the file off disk as UTF-8. */
  readonly readFile?: ((path: string) => string) | undefined;
}

/**
 * The brand this deployment renders.
 *
 * Resolved once per process by `brandOnHand`; call that from a page
 * rather than this, unless a test wants a specific environment.
 *
 * @throws {BrandError} when `APP_ENV` names an environment this
 * application cannot read, when a deployed environment names no brand,
 * or when the named brand cannot be read or does not match the schema.
 */
export function loadBrand(options: LoadBrandOptions = {}): LoadedBrand {
  const appEnv = parseAppEnv(options.appEnv);
  if (appEnv === null) {
    throw new BrandError(
      `APP_ENV is ${JSON.stringify(options.appEnv)}, which is neither ${JSON.stringify(APP_ENV_DEV)} nor ${JSON.stringify(APP_ENV_PROD)}. A value this application cannot read is not development: it would print the fixture brand's entity and address as this service's own provider identification.`,
    );
  }

  const dir = options.brandDir?.trim() ?? '';
  if (dir === '') {
    if (appEnv === APP_ENV_PROD) {
      throw new BrandError(
        'BRAND_DIR is not set in a deployed environment (APP_ENV=prod): the legal notices would name the fixture brand\'s entity, address and support addresses, which identify a company that does not exist. Set BRAND_DIR to the directory holding this deployment\'s brand.json.',
      );
    }
    return { brand: parseBrand(fixtureBrandJson), source: BRAND_SOURCE_FIXTURE };
  }

  const path = join(dir, BRAND_FILE_NAME);
  const read = options.readFile ?? ((target: string) => readFileSync(target, 'utf8'));
  let raw: string;
  try {
    raw = read(path);
  } catch (cause) {
    // A named brand that cannot be read is a startup failure, not a
    // fallback: the deployment meant the brand it named, and rendering
    // the fixture instead would print the wrong company while an
    // operator believed the brand was configured. Same stance as the Go
    // binary takes on an unreadable BRAND_DIR.
    throw new BrandError(
      `BRAND_DIR=${dir}: ${path} could not be read: ${cause instanceof Error ? cause.message : String(cause)}`,
    );
  }
  return { brand: parseBrand(raw), source: BRAND_SOURCE_DEPLOYMENT };
}

let resolved: LoadedBrand | null = null;

/**
 * The brand this process renders, resolved once.
 *
 * A brand file does not change while a container runs, so re-reading it
 * per request would buy nothing and put a synchronous disk read on every
 * page. The first caller pays for it; a throw is not cached, so a
 * misconfigured deployment fails the same way on every request rather
 * than only on the first.
 */
export function brandOnHand(options: LoadBrandOptions = {}): LoadedBrand {
  if (resolved === null) {
    resolved = loadBrand(options);
  }
  return resolved;
}

/** Forgets the resolved brand. For tests; no page calls this. */
export function forgetBrandOnHand(): void {
  resolved = null;
}
