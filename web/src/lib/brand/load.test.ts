import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

import { beforeEach, describe, expect, it } from 'vitest';

import { BrandError } from './index';
import {
  BRAND_SOURCE_DEPLOYMENT,
  BRAND_SOURCE_FIXTURE,
  brandOnHand,
  forgetBrandOnHand,
  loadBrand,
} from './load';

const fixturePath = fileURLToPath(new URL('./fixture.brand.json', import.meta.url));
const fixtureJson = readFileSync(fixturePath, 'utf8');

/** A directory whose brand.json is the fixture's bytes, without a disk. */
function reader(byPath: Record<string, string>): (path: string) => string {
  return (path) => {
    // The loader joins with the platform separator; compare on both.
    const key = path.replace(/\\/g, '/');
    const found = byPath[key];
    if (found === undefined) {
      throw new Error(`ENOENT: no such file or directory, open '${path}'`);
    }
    return found;
  };
}

describe('the brand a deployment names', () => {
  it('reads brand.json out of the directory BRAND_DIR names', () => {
    const loaded = loadBrand({
      brandDir: '/srv/brand',
      appEnv: 'prod',
      readFile: reader({ '/srv/brand/brand.json': fixtureJson }),
    });

    expect(loaded.source).toBe(BRAND_SOURCE_DEPLOYMENT);
    expect(loaded.brand.legal.entity).toBe('Zephyra Fixture Kooperativ AB');
  });

  it('refuses a named brand it cannot read, rather than falling back', () => {
    // The deployment meant the brand it named. Rendering the fixture
    // instead would print the wrong company while an operator believed
    // the brand was configured.
    expect(() =>
      loadBrand({ brandDir: '/srv/brand', appEnv: 'dev', readFile: reader({}) }),
    ).toThrow(BrandError);
  });

  it('refuses a named brand whose file does not match the schema', () => {
    expect(() =>
      loadBrand({
        brandDir: '/srv/brand',
        appEnv: 'dev',
        readFile: reader({ '/srv/brand/brand.json': '{"id":"half"}' }),
      }),
    ).toThrow(BrandError);
  });
});

describe('a deployment that names no brand', () => {
  it('falls back to the fixture in development, and says which it is', () => {
    const loaded = loadBrand({ appEnv: 'dev' });

    expect(loaded.source).toBe(BRAND_SOURCE_FIXTURE);
    expect(loaded.brand.id).toBe('zephyra');
  });

  it('treats blank and whitespace the same as unset', () => {
    expect(loadBrand({ brandDir: '', appEnv: 'dev' }).source).toBe(BRAND_SOURCE_FIXTURE);
    expect(loadBrand({ brandDir: '   ', appEnv: 'dev' }).source).toBe(BRAND_SOURCE_FIXTURE);
  });

  it('refuses in a deployed environment', () => {
    // The Impressum is a statement about who is legally responsible.
    // A fixture there is a false statement, not a placeholder.
    expect(() => loadBrand({ appEnv: 'prod' })).toThrow(BrandError);
  });

  it('refuses an APP_ENV it cannot read, rather than assuming development', () => {
    expect(() => loadBrand({ appEnv: 'production' })).toThrow(BrandError);
  });
});

describe('the brand on hand', () => {
  beforeEach(() => {
    forgetBrandOnHand();
  });

  it('resolves once and answers every later caller from the same value', () => {
    const first = brandOnHand({ appEnv: 'dev' });
    const second = brandOnHand({ appEnv: 'prod' });

    // The second call's options are ignored because the first already
    // answered — a brand file does not change while a container runs.
    expect(second).toBe(first);
  });

  it('does not cache a refusal, so a misconfiguration fails on every request', () => {
    expect(() => brandOnHand({ appEnv: 'prod' })).toThrow(BrandError);
    expect(() => brandOnHand({ appEnv: 'prod' })).toThrow(BrandError);
    expect(brandOnHand({ appEnv: 'dev' }).source).toBe(BRAND_SOURCE_FIXTURE);
  });
});

describe('the fixture brand', () => {
  it('is byte-identical to the one the Go package tests against', () => {
    // Two copies of one fixture that drift apart would make the rebrand
    // proof (CB-T127) pass on this side and fail on the other. The Go
    // file is the original; this one is the copy, and this test is what
    // makes the copy safe.
    const goFixture = readFileSync(
      fileURLToPath(new URL('../../../../internal/platform/brand/testdata/fixture/brand.json', import.meta.url)),
      'utf8',
    );

    expect(fixtureJson).toBe(goFixture);
  });
});
