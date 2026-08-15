/**
 * Accounts and consent (US6, FR-011).
 *
 * The schema's rule is the whole point of this module: consent is **never
 * a boolean**. It is one dated record per purpose, revocation is a date on
 * that record rather than a deletion, and re-granting opens a new record
 * while the old one stands. `consent` (migration 0001) enforces exactly
 * that — append-only history, identity and grant frozen, revocation
 * one-way, one active row per purpose.
 *
 * No endpoint exists: the contract makes reader registration and the
 * consent grant/revoke endpoints conditional on the registration UI
 * surviving the cut line, and the capability lives in the schema
 * regardless (T031 #34). This module is the seam.
 */

/** The purposes the alpha asks about. Each is its own consent record. */
export const CONSENT_PURPOSES = ['newsletter', 'analytics', 'product_news'] as const;

/** A purpose a reader can consent to. */
export type ConsentPurpose = (typeof CONSENT_PURPOSES)[number];

/**
 * One row of `consent`. `revoked_at` set means this record is closed —
 * the record itself is never removed or rewritten.
 */
export interface ConsentRecord {
  readonly purpose: ConsentPurpose;
  /** ISO 8601 — when this record was granted. */
  readonly granted_at: string;
  /** ISO 8601, or null while the record is the active one. */
  readonly revoked_at: string | null;
}

/** Whether a purpose is currently granted: exactly one unrevoked record. */
export function isGranted(
  history: readonly ConsentRecord[],
  purpose: ConsentPurpose,
): boolean {
  return history.some((record) => record.purpose === purpose && record.revoked_at === null);
}

/**
 * The history for display, newest first. Every record is shown, including
 * closed ones — that a purpose was once granted and later revoked is part
 * of what the reader is entitled to see.
 */
export function historyNewestFirst(
  history: readonly ConsentRecord[],
): readonly ConsentRecord[] {
  // Compare instants, not strings: two records can describe the same
  // moment as `Z` and `+00:00`, or sit in different offsets, and a
  // lexicographic sort would then order a consent history wrongly — the
  // one thing this list exists to get right.
  return [...history].sort(
    (a, b) => Date.parse(b.granted_at) - Date.parse(a.granted_at),
  );
}

/**
 * The outcome of granting or revoking. As everywhere else in this app,
 * `recorded: false` is the honest answer while nothing can be written —
 * a consent UI that appears to record a choice it did not record would be
 * the worst possible thing to fake.
 */
export interface ConsentOutcome {
  readonly recorded: boolean;
  readonly reason?: string;
}

/** The outcome of registering. */
export interface RegistrationOutcome {
  readonly recorded: boolean;
  readonly account_id?: string;
  readonly reason?: string;
}

/** The account surface the registration screen consumes. */
export interface AccountApi {
  consentHistory(): Promise<readonly ConsentRecord[]>;
  setConsent(purpose: ConsentPurpose, granted: boolean): Promise<ConsentOutcome>;
}

const NOT_WIRED_CONSENT =
  'Accounts are not wired yet (T031), so no consent record was written. The schema keeps consent as dated per-purpose records; nothing here was stored.';

/**
 * A reader's consent history, showing the shape the schema requires:
 * newsletter granted, revoked, and granted again — three records for one
 * purpose, not one switch — plus a closed analytics pair.
 */
export const CONSENT_FIXTURE: readonly ConsentRecord[] = [
  { purpose: 'newsletter', granted_at: '2026-03-02T09:14:00Z', revoked_at: '2026-05-18T18:02:00Z' },
  { purpose: 'newsletter', granted_at: '2026-08-14T07:30:00Z', revoked_at: null },
  { purpose: 'analytics', granted_at: '2026-03-02T09:14:00Z', revoked_at: '2026-03-09T11:20:00Z' },
];

/**
 * Builds the client. Only the fixture path exists today; when the consent
 * endpoints land they slot in here without the screen changing.
 */
export function createAccountApi(_baseUrl?: string | undefined): AccountApi {
  return {
    consentHistory(): Promise<readonly ConsentRecord[]> {
      return Promise.resolve(CONSENT_FIXTURE);
    },
    setConsent(): Promise<ConsentOutcome> {
      return Promise.resolve({ recorded: false, reason: NOT_WIRED_CONSENT });
    },
  };
}
