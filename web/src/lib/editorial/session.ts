/**
 * The editor identity behind the editorial screens.
 *
 * The approver is never anonymous: `article.approved_by` is NOT NULL and
 * migration 0002 additionally requires that account to hold the editor
 * role, so every editorial screen names who is signed in. Today that name
 * comes from a fixture — Supabase Auth and the JWT round trip are T022's
 * other half (research D7), and `internal/identity` already validates
 * tokens although no endpoint consumes them yet.
 *
 * This module is the single seam: when auth lands, `editorSession()` reads
 * the Supabase session and returns the real account and access token, and
 * nothing else on the screens changes.
 */

/** The signed-in person, as the editorial screens need them. */
export interface EditorSession {
  readonly displayName: string;
  readonly email: string;
  /** Mirrors `account.role`; only 'editor' may approve (0002 trigger). */
  readonly role: 'editor' | 'reader';
  /** The bearer token for the editorial API; null while auth is unwired. */
  readonly token: string | null;
  /**
   * False while the identity is a local placeholder. The screens surface
   * this rather than implying a real sign-in — a named approver that
   * nobody authenticated is exactly the claim this product must not make.
   */
  readonly authenticated: boolean;
}

/** The placeholder editor: one of the mockups' fictional names. */
const PREVIEW_EDITOR: EditorSession = {
  displayName: 'Eleni Papadaki',
  email: 'eleni@epiloyes.example',
  role: 'editor',
  token: null,
  authenticated: false,
};

/**
 * The current editor session. Takes the request so the Supabase
 * implementation can read cookies without changing any caller.
 */
export function editorSession(_request: Request): EditorSession {
  return PREVIEW_EDITOR;
}
