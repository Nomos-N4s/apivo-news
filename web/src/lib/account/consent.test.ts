import { describe, expect, it } from 'vitest';

import {
  CONSENT_FIXTURE,
  CONSENT_PURPOSES,
  createAccountApi,
  historyNewestFirst,
  isGranted,
  type ConsentRecord,
} from './consent';

describe('consent is records, never a boolean (FR-011)', () => {
  it('reads granted as exactly one unrevoked record for the purpose', () => {
    expect(isGranted(CONSENT_FIXTURE, 'newsletter')).toBe(true);
    expect(isGranted(CONSENT_FIXTURE, 'analytics')).toBe(false);
    expect(isGranted(CONSENT_FIXTURE, 'product_news')).toBe(false);
  });

  it('keeps a revoked record standing rather than deleting it', () => {
    const newsletter = CONSENT_FIXTURE.filter((record) => record.purpose === 'newsletter');
    expect(newsletter).toHaveLength(2);
    expect(newsletter.filter((record) => record.revoked_at !== null)).toHaveLength(1);
  });

  it('holds more records than purposes — the point of the screen', () => {
    expect(CONSENT_FIXTURE.length).toBeGreaterThan(
      new Set(CONSENT_FIXTURE.map((record) => record.purpose)).size,
    );
  });

  it('treats a purpose with no record at all as not granted', () => {
    expect(isGranted([], 'newsletter')).toBe(false);
  });

  it('a re-grant after a revoke reads as granted again', () => {
    const history: ConsentRecord[] = [
      { purpose: 'analytics', granted_at: '2026-01-01T00:00:00Z', revoked_at: '2026-02-01T00:00:00Z' },
      { purpose: 'analytics', granted_at: '2026-03-01T00:00:00Z', revoked_at: null },
    ];
    expect(isGranted(history, 'analytics')).toBe(true);
    expect(history).toHaveLength(2);
  });
});

describe('historyNewestFirst', () => {
  it('orders by grant date, newest first, keeping every record', () => {
    const ordered = historyNewestFirst(CONSENT_FIXTURE);
    expect(ordered).toHaveLength(CONSENT_FIXTURE.length);
    const dates = ordered.map((record) => record.granted_at);
    expect(dates).toEqual([...dates].sort().reverse());
  });

  it('does not mutate the input', () => {
    const before = [...CONSENT_FIXTURE];
    historyNewestFirst(CONSENT_FIXTURE);
    expect([...CONSENT_FIXTURE]).toEqual(before);
  });
});

describe('the account client', () => {
  it('never fakes a consent record', async () => {
    const outcome = await createAccountApi().setConsent('newsletter', false);
    expect(outcome.recorded).toBe(false);
    expect(outcome.reason).toContain('no consent record was written');
  });

  it('answers the history', async () => {
    await expect(createAccountApi().consentHistory()).resolves.toHaveLength(
      CONSENT_FIXTURE.length,
    );
  });

  it('names every alpha purpose', () => {
    expect(CONSENT_PURPOSES).toContain('newsletter');
    expect(CONSENT_PURPOSES).toContain('analytics');
    expect(CONSENT_PURPOSES).toContain('product_news');
  });
});
