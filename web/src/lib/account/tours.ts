/**
 * The account's tour progress, over the API.
 *
 * Used from the SERVER only — Astro frontmatter and the endpoint under
 * pages/api. The bearer token in this product never reaches the browser:
 * every editorial screen builds its client in frontmatter
 * (`createEditorialApi(API_BASE_URL, session.token)`) and renders the
 * result, and a guided tour is not a reason to start handing tokens to
 * page scripts. So the browser reads its starting cursor from an attribute
 * the server rendered, and writes through a same-origin endpoint that
 * holds the session on this side of the wire.
 *
 * Same seam as the other clients: `baseUrl` empty means no API is
 * configured, and every call degrades to "no progress recorded" rather
 * than throwing. A tour is the least important thing on any of these
 * screens, and it must never be the reason one fails to render.
 */

/** A tour id to its cursor: a step index as text, or 'done'. */
export type TourProgress = Readonly<Record<string, string>>;

/** Nothing recorded — the answer whenever the API cannot be asked. */
export const NO_PROGRESS: TourProgress = Object.freeze({});

/** What the API accepts as a cursor; mirrors the Go handler's pattern. */
const CURSOR = /^(done|[0-9]{1,4})$/;

/** What the API accepts as a tour id; mirrors the Go handler's pattern. */
const TOUR_ID = /^[a-z][a-z0-9-]{0,63}$/;

export function isCursor(value: string): boolean {
  return CURSOR.test(value);
}

export function isTourId(value: string): boolean {
  return TOUR_ID.test(value);
}

/**
 * Reads the response body of GET /api/v1/account/tours into a flat map,
 * discarding anything that is not a string value.
 *
 * Exported for its own test: the shape crosses a network boundary from a
 * column that clients write, and "it will be a flat object" is an
 * assumption, not a guarantee.
 */
export function parseTours(body: unknown): TourProgress {
  if (typeof body !== 'object' || body === null) {
    return NO_PROGRESS;
  }
  const tours = (body as { tours?: unknown }).tours;
  if (typeof tours !== 'object' || tours === null || Array.isArray(tours)) {
    return NO_PROGRESS;
  }
  const out: Record<string, string> = {};
  for (const [key, value] of Object.entries(tours as Record<string, unknown>)) {
    if (typeof value === 'string' && isTourId(key) && isCursor(value)) {
      out[key] = value;
    }
  }
  return out;
}

/**
 * This account's tour progress, or nothing recorded.
 *
 * Never throws and never rejects: no API configured, no token, a non-2xx,
 * a body in the wrong shape and a network failure all mean the same thing
 * to the caller — start the tour from the beginning — and distinguishing
 * them would only give the screen a decision it has no way to act on.
 */
export async function readTourProgress(
  baseUrl: string | undefined,
  token: string | null,
  fetchImpl: typeof fetch = fetch,
): Promise<TourProgress> {
  if (baseUrl === undefined || baseUrl === '' || token === null || token === '') {
    return NO_PROGRESS;
  }
  try {
    const response = await fetchImpl(`${baseUrl.replace(/\/$/, '')}/api/v1/account/tours`, {
      headers: { Authorization: `Bearer ${token}`, Accept: 'application/json' },
    });
    if (!response.ok) {
      return NO_PROGRESS;
    }
    return parseTours(await response.json());
  } catch {
    return NO_PROGRESS;
  }
}

/**
 * Records one cursor. Reports whether the API stored it, so the caller can
 * decide whether the browser's own copy is now the only one — it does not
 * throw, for the same reason the read does not.
 */
export async function writeTourProgress(
  baseUrl: string | undefined,
  token: string | null,
  tourId: string,
  cursor: string,
  fetchImpl: typeof fetch = fetch,
): Promise<boolean> {
  if (baseUrl === undefined || baseUrl === '' || token === null || token === '') {
    return false;
  }
  // Checked here as well as by the API. The values come from a page
  // script, and a request the server is certain to refuse is not worth
  // making — nor worth the log line it would produce on the other side.
  if (!isTourId(tourId) || !isCursor(cursor)) {
    return false;
  }
  try {
    const response = await fetchImpl(
      `${baseUrl.replace(/\/$/, '')}/api/v1/account/tours/${tourId}`,
      {
        method: 'PUT',
        headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' },
        body: JSON.stringify({ cursor }),
      },
    );
    return response.ok;
  } catch {
    return false;
  }
}
