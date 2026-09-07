/**
 * Becoming an account here, on the strength of a token the provider verified
 * (issue #567, against the endpoint #544 landed).
 *
 * The auth provider says who somebody is. It does not make them anybody to
 * this api: every cashback route looks the caller up in `account` and answers
 * 401 when there is no row (`ErrUnknownAccount`, FR-023). So a member who has
 * just signed in successfully is, until this call, a member whose wallet,
 * catalogue and click-out all fail — the worst possible state, because
 * nothing about it looks broken.
 *
 * `POST /api/v1/account` takes no body. The subject and the email come out of
 * the token, which is the only place they can honestly come from: a body
 * would be a client asserting an identity the token already proves. It never
 * changes a role, so calling it on every sign-in is safe and a second call is
 * a no-op.
 */

/** The caller's own account row, as the api reports it. */
export interface Account {
  readonly id: string;
  readonly email: string;
  readonly display_name: string;
  readonly role: 'reader' | 'editor' | 'operator';
}

/**
 * What became of a registration.
 *
 * `existing` and `registered` are both success and are kept apart because
 * only one of them is a person's first moment here; nothing acts on the
 * difference yet, and a caller that wants to greet somebody will.
 *
 * `email_taken` is the only one that is a refusal ABOUT the member rather
 * than about the deployment. The subject is the identity and the email is
 * unique, so this is one person arriving under a second provider account
 * with an address already spoken for — a thing to name, never to merge.
 */
export type RegistrationOutcome =
  | 'registered'
  | 'existing'
  | 'email_taken'
  | 'no_email'
  | 'unavailable';

export interface Registration {
  readonly outcome: RegistrationOutcome;
  /** The row, where the api returned one. */
  readonly account: Account | null;
}

/**
 * Register the holder of this token, or read back the account already there.
 *
 * **It never throws.** A sign-in that has already succeeded must not be
 * undone by this call failing: the session is real, the person is signed in,
 * and the worst honest answer is `unavailable` — which the caller turns into
 * "you are signed in and the money surfaces are not ready for you yet"
 * rather than into a stack trace on a page somebody reached from their mail.
 *
 * An unset `baseUrl` is `unavailable` too, and is an ordinary state: a
 * development run or a pull-request preview has no api to register against,
 * and every cashback surface there answers from fixtures anyway.
 */
export async function registerAccount(
  baseUrl: string | undefined,
  token: string,
  fetchImpl: typeof fetch = fetch,
): Promise<Registration> {
  if (baseUrl === undefined || baseUrl === '' || token === '') {
    return { outcome: 'unavailable', account: null };
  }
  const url = `${baseUrl.replace(/\/+$/, '')}/api/v1/account`;
  let response: Response;
  try {
    response = await fetchImpl(url, {
      method: 'POST',
      headers: { Accept: 'application/json', Authorization: `Bearer ${token}` },
    });
  } catch {
    // The api container is down, or DNS is not answering, or the deployment
    // points at nothing. None of that is the member's problem right now.
    return { outcome: 'unavailable', account: null };
  }

  switch (response.status) {
    case 200:
      return { outcome: 'existing', account: await accountOf(response) };
    case 201:
      return { outcome: 'registered', account: await accountOf(response) };
    case 400:
      return { outcome: 'no_email', account: null };
    case 409:
      return { outcome: 'email_taken', account: null };
    default:
      // 401 and 500 land here together on purpose. A 401 at this point means
      // the token the exchange just produced did not verify at the api —
      // clock skew, or a JWKS pointing at another project — and a member can
      // do nothing about either. Both are "not now", not "not ever".
      return { outcome: 'unavailable', account: null };
  }
}

/** The body, or null when it is not the shape the contract promises. */
async function accountOf(response: Response): Promise<Account | null> {
  let body: unknown;
  try {
    body = await response.json();
  } catch {
    return null;
  }
  if (typeof body !== 'object' || body === null) {
    return null;
  }
  const row = body as Record<string, unknown>;
  if (typeof row['id'] !== 'string' || typeof row['email'] !== 'string') {
    return null;
  }
  return body as Account;
}
