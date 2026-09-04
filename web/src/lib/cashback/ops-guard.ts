import type { EditorSession } from '../editorial/session';
import type { CashbackSource } from './api';

/**
 * Whether this request may act on an operator queue.
 *
 * Two different questions, deliberately kept apart:
 *
 *   - **May they act?** Only an authenticated operator. The API enforces
 *     this too, and the database enforces it under that (migration 0019's
 *     trigger reads the role `FOR SHARE`), so this check is the third of
 *     three and the only one that can explain itself to the person at the
 *     screen. It is never the only one.
 *   - **May they look?** With a real API, no: the queue rows carry members'
 *     account ids, held amounts and network references, and a non-operator
 *     is answered 403 by the API anyway. With fixtures there is nothing to
 *     leak and no deployment with auth to sign in to, so the preview
 *     renders and says what it is.
 *
 * Collapsing the two would make the operator screens unreviewable in
 * development, or would show real queue rows to a reader. Neither is
 * acceptable, and they are not the same decision.
 */
export interface OpsAccess {
  /** Whether the decision forms are offered and their handlers will run. */
  readonly mayAct: boolean;
  /** Whether the queue contents render at all. */
  readonly mayRead: boolean;
  /** Whether to tell the person that the role is what is missing. */
  readonly blocked: boolean;
}

export function opsAccess(session: EditorSession, source: CashbackSource): OpsAccess {
  const mayAct = session.authenticated && session.role === 'operator';
  const preview = source === 'fixture';
  return {
    mayAct,
    mayRead: mayAct || preview,
    blocked: !mayAct && !preview,
  };
}
