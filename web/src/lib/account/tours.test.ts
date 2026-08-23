import { describe, expect, it } from 'vitest';

import {
  isCursor,
  isTourId,
  NO_PROGRESS,
  parseTours,
  readTourProgress,
  writeTourProgress,
} from './tours';

const ok = (body: unknown): typeof fetch =>
  (async () => new Response(JSON.stringify(body), { status: 200 })) as unknown as typeof fetch;

describe('parseTours', () => {
  it('reads a flat document', () => {
    expect(parseTours({ tours: { editor: '7', reader: 'done' } })).toEqual({
      editor: '7',
      reader: 'done',
    });
  });

  // The column is written by clients. A value that is not a cursor, or a
  // key that is not a tour id, is somebody else's bug arriving here — it
  // must not become a step index.
  it('drops entries outside the shape', () => {
    expect(
      parseTours({
        tours: { editor: '7', Bad: '1', worse: 'yes', nested: { a: 1 }, numeric: 3 },
      }),
    ).toEqual({ editor: '7' });
  });

  it.each([null, undefined, 42, 'tours', [], { tours: null }, { tours: [] }, {}])(
    'treats %o as nothing recorded',
    (body) => {
      expect(parseTours(body)).toEqual(NO_PROGRESS);
    },
  );
});

describe('readTourProgress', () => {
  it('returns the document', async () => {
    await expect(readTourProgress('https://api.test', 'tok', ok({ tours: { editor: '2' } }))).resolves.toEqual({
      editor: '2',
    });
  });

  // A tour must never be the reason an editorial screen fails to render,
  // so every one of these is "no progress recorded" rather than a throw.
  it('is empty without an API configured', async () => {
    await expect(readTourProgress('', 'tok')).resolves.toEqual(NO_PROGRESS);
    await expect(readTourProgress(undefined, 'tok')).resolves.toEqual(NO_PROGRESS);
  });

  it('is empty without a token', async () => {
    await expect(readTourProgress('https://api.test', null)).resolves.toEqual(NO_PROGRESS);
    await expect(readTourProgress('https://api.test', '')).resolves.toEqual(NO_PROGRESS);
  });

  it('is empty on a non-2xx', async () => {
    const failing = (async () => new Response('nope', { status: 500 })) as unknown as typeof fetch;
    await expect(readTourProgress('https://api.test', 'tok', failing)).resolves.toEqual(NO_PROGRESS);
  });

  it('is empty when the request throws', async () => {
    const boom = (async () => {
      throw new Error('offline');
    }) as unknown as typeof fetch;
    await expect(readTourProgress('https://api.test', 'tok', boom)).resolves.toEqual(NO_PROGRESS);
  });

  it('is empty when the body is not JSON', async () => {
    const junk = (async () => new Response('<html>', { status: 200 })) as unknown as typeof fetch;
    await expect(readTourProgress('https://api.test', 'tok', junk)).resolves.toEqual(NO_PROGRESS);
  });

  it('sends the token as a bearer and does not double the slash', async () => {
    let seen: { url: string; auth: string | null } | null = null;
    const spy = (async (url: string | URL | Request, init?: RequestInit) => {
      const headers = new Headers(init?.headers);
      seen = { url: String(url), auth: headers.get('Authorization') };
      return new Response(JSON.stringify({ tours: {} }), { status: 200 });
    }) as unknown as typeof fetch;
    await readTourProgress('https://api.test/', 'tok', spy);
    expect(seen!.url).toBe('https://api.test/api/v1/account/tours');
    expect(seen!.auth).toBe('Bearer tok');
  });
});

describe('writeTourProgress', () => {
  it('reports what the API said', async () => {
    const created = (async () => new Response(null, { status: 204 })) as unknown as typeof fetch;
    await expect(writeTourProgress('https://api.test', 'tok', 'editor', '3', created)).resolves.toBe(true);
    const refused = (async () => new Response(null, { status: 409 })) as unknown as typeof fetch;
    await expect(writeTourProgress('https://api.test', 'tok', 'editor', '3', refused)).resolves.toBe(false);
  });

  // A request the server is certain to refuse is not worth making, nor
  // worth the log line it would produce on the other side.
  it('refuses a bad id or cursor without calling out', async () => {
    let called = false;
    const spy = (async () => {
      called = true;
      return new Response(null, { status: 204 });
    }) as unknown as typeof fetch;
    await expect(writeTourProgress('https://api.test', 'tok', 'Editor', '3', spy)).resolves.toBe(false);
    await expect(writeTourProgress('https://api.test', 'tok', 'editor', 'nope', spy)).resolves.toBe(false);
    expect(called).toBe(false);
  });

  it('is false when the request throws', async () => {
    const boom = (async () => {
      throw new Error('offline');
    }) as unknown as typeof fetch;
    await expect(writeTourProgress('https://api.test', 'tok', 'editor', '3', boom)).resolves.toBe(false);
  });
});

// These mirror the Go handler's patterns. Drift here means the browser
// sends what the API refuses, or refuses what it would accept.
describe('shape guards', () => {
  it.each(['done', '0', '7', '9999'])('accepts cursor %o', (c) => expect(isCursor(c)).toBe(true));
  it.each(['', ' 1', '-1', '1.5', '10000', 'DONE', 'finished'])('refuses cursor %o', (c) =>
    expect(isCursor(c)).toBe(false),
  );
  it.each(['editor', 'reader-setup', 'a'])('accepts id %o', (i) => expect(isTourId(i)).toBe(true));
  it.each(['', 'Editor', '1editor', '-editor', 'editor.tour', 'editor_tour'])('refuses id %o', (i) =>
    expect(isTourId(i)).toBe(false),
  );
});
