import { describe, expect, it } from 'vitest';

import { isSecureRequest } from '../secure-request';
import {
  astroCookieOptions,
  editorSession,
  editorSessionFrom,
  NO_EDITOR_SESSION,
  parseCookieHeader,
  rememberEditorSession,
  supabaseConfig,
  type SupabaseUserClaims,
} from './session';

const EDITOR: SupabaseUserClaims = {
  email: 'eleni@epiloyes.example',
  user_metadata: { display_name: 'Eleni Papadaki' },
  app_metadata: { role: 'editor' },
};

describe('editorSession', () => {
  it('reports nobody signed in for a request the middleware never resolved', () => {
    const session = editorSession(new Request('http://localhost/el/editor'));
    expect(session.authenticated).toBe(false);
    expect(session.token).toBeNull();
    // No placeholder name: the chrome prints who is at the keyboard, and
    // here it does not know.
    expect(session.displayName).toBe('');
    expect(session.email).toBe('');
  });

  it('gives a resolved request back the identity that was filed for it', () => {
    const request = new Request('http://localhost/el/editor');
    rememberEditorSession(request, editorSessionFrom(EDITOR, 'access-token'));
    expect(editorSession(request).displayName).toBe('Eleni Papadaki');
    expect(editorSession(request).token).toBe('access-token');
  });

  it('keeps the identity of one request out of the next one', () => {
    const mine = new Request('http://localhost/el/editor');
    rememberEditorSession(mine, editorSessionFrom(EDITOR, 'access-token'));
    expect(editorSession(new Request('http://localhost/el/editor')).authenticated).toBe(false);
  });
});

describe('editorSessionFrom', () => {
  it('names the account, its email and the token it will call the API with', () => {
    const session = editorSessionFrom(EDITOR, 'access-token');
    expect(session.displayName).toBe('Eleni Papadaki');
    expect(session.email).toBe('eleni@epiloyes.example');
    expect(session.token).toBe('access-token');
    expect(session.authenticated).toBe(true);
    expect(session.role).toBe('editor');
  });

  it('never reports an authenticated session without a token to back it', () => {
    // A name without a token would put a person on screen while every
    // call to the editorial API went out unauthenticated.
    for (const token of [null, undefined, '']) {
      const session = editorSessionFrom(EDITOR, token);
      expect(session.authenticated).toBe(false);
      expect(session.token).toBeNull();
      expect(session.displayName).toBe('');
    }
  });

  it('reports nobody signed in when there is no user, token or not', () => {
    expect(editorSessionFrom(null, 'access-token')).toEqual(NO_EDITOR_SESSION);
    expect(editorSessionFrom(undefined, 'access-token')).toEqual(NO_EDITOR_SESSION);
  });

  it('falls back through the display-name spellings, then to the email', () => {
    const email = 'markus@epiloyes.example';
    expect(
      editorSessionFrom({ email, user_metadata: { full_name: 'Markus Bauer' } }, 't').displayName,
    ).toBe('Markus Bauer');
    expect(editorSessionFrom({ email, user_metadata: { name: 'Markus B.' } }, 't').displayName).toBe(
      'Markus B.',
    );
    expect(editorSessionFrom({ email, user_metadata: {} }, 't').displayName).toBe(email);
    expect(editorSessionFrom({ email }, 't').displayName).toBe(email);
    // A blank metadata name is not a name.
    expect(editorSessionFrom({ email, user_metadata: { display_name: '  ' } }, 't').displayName).toBe(
      email,
    );
  });

  it('reads editor authority only from an explicit editor claim', () => {
    // The database is the authority on account.role; anything the token
    // does not plainly assert reads as reader, because under-claiming
    // costs a sign-in and over-claiming names an approver nobody made.
    expect(editorSessionFrom({ app_metadata: { role: 'editor' } }, 't').role).toBe('editor');
    expect(editorSessionFrom({ app_metadata: { role: 'reader' } }, 't').role).toBe('reader');
    expect(editorSessionFrom({ app_metadata: { role: 'admin' } }, 't').role).toBe('reader');
    expect(editorSessionFrom({ app_metadata: {} }, 't').role).toBe('reader');
    expect(editorSessionFrom({}, 't').role).toBe('reader');
  });

  it('leaves the email empty rather than inventing one', () => {
    expect(editorSessionFrom({ email: null }, 't').email).toBe('');
    expect(editorSessionFrom({}, 't').email).toBe('');
  });
});

describe('supabaseConfig', () => {
  it('answers a configuration only when both halves are present', () => {
    expect(supabaseConfig('https://project.supabase.co', 'anon-key')).toEqual({
      url: 'https://project.supabase.co',
      anonKey: 'anon-key',
    });
  });

  it('treats half a configuration as none — a URL cannot authenticate anyone', () => {
    expect(supabaseConfig('https://project.supabase.co', undefined)).toBeNull();
    expect(supabaseConfig(undefined, 'anon-key')).toBeNull();
    expect(supabaseConfig('', '')).toBeNull();
    expect(supabaseConfig('  ', 'anon-key')).toBeNull();
    expect(supabaseConfig('https://project.supabase.co', '  ')).toBeNull();
  });
});

describe('astroCookieOptions', () => {
  it('forces httpOnly on, whatever the SDK asked', () => {
    // The session cookie must stay out of page-script reach: no code in
    // this application reads it from the browser, so an injected script
    // must not be able to either. The SDK's own default is false.
    expect(astroCookieOptions({}, true).httpOnly).toBe(true);
    expect(astroCookieOptions({ httpOnly: false }, true).httpOnly).toBe(true);
  });

  it('takes secure from the caller rather than hard-coding it', () => {
    // https deployments get an https-only cookie; the plain-http dev
    // stack still works.
    expect(astroCookieOptions({}, true).secure).toBe(true);
    expect(astroCookieOptions({}, false).secure).toBe(false);
    // And the SDK cannot opt the cookie out of it.
    expect(astroCookieOptions({ secure: false }, true).secure).toBe(true);
  });

  // The composition supabase.ts performs, stated end to end: the request
  // the container sees is http:// on a deployed site, because the Worker
  // proxies it that way, and the session cookie must still be Secure. A
  // refresh token without it rides the next cleartext request to the same
  // host, and whoever captures it can POST an approval under a real
  // editor's name.
  it('writes a Secure session cookie on a deployed request whose URL is http', () => {
    const proxied = new Request('http://news.example/el/editor');
    expect(new URL(proxied.url).protocol).toBe('http:');
    expect(astroCookieOptions({}, isSecureRequest(proxied, 'prod')).secure).toBe(true);
  });

  it('passes through the options the SDK set and omits the ones it did not', () => {
    const expires = new Date('2026-08-16T00:00:00Z');
    expect(
      astroCookieOptions(
        { domain: 'epiloyes.example', path: '/', expires, maxAge: 3600, sameSite: 'lax' },
        true,
      ),
    ).toEqual({
      domain: 'epiloyes.example',
      path: '/',
      expires,
      maxAge: 3600,
      sameSite: 'lax',
      httpOnly: true,
      secure: true,
    });
    // An absent option stays absent rather than becoming `undefined`:
    // Astro writes the keys it is given.
    expect(Object.keys(astroCookieOptions({}, false))).toEqual(['httpOnly', 'secure']);
  });
});

describe('parseCookieHeader', () => {
  it('reads every cookie the request carried', () => {
    expect(parseCookieHeader('sb-a-auth-token.0=first; sb-a-auth-token.1=second')).toEqual([
      { name: 'sb-a-auth-token.0', value: 'first' },
      { name: 'sb-a-auth-token.1', value: 'second' },
    ]);
  });

  it('decodes the percent-encoding cookies are written with', () => {
    expect(parseCookieHeader('sb=base64-%7Bvalue%7D')).toEqual([
      { name: 'sb', value: 'base64-{value}' },
    ]);
  });

  it('passes a malformed value through rather than throwing on the request', () => {
    expect(parseCookieHeader('sb=%E0%A4%A')).toEqual([{ name: 'sb', value: '%E0%A4%A' }]);
  });

  it('skips fragments that are not a name and a value', () => {
    expect(parseCookieHeader('novalue; =orphan; ok=1')).toEqual([{ name: 'ok', value: '1' }]);
  });

  it('has nothing to read without a header', () => {
    expect(parseCookieHeader(null)).toEqual([]);
    expect(parseCookieHeader('   ')).toEqual([]);
  });
});
