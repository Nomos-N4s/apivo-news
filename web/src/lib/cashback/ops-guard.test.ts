import { describe, expect, it } from 'vitest';

import { opsAccess } from './ops-guard';
import type { EditorSession } from '../editorial/session';

const session = (
  role: EditorSession['role'],
  authenticated = true,
): EditorSession => ({
  displayName: 'N. Alexiou',
  email: 'ops@example.invalid',
  role,
  token: authenticated ? 'jwt' : null,
  authenticated,
});

describe('opsAccess', () => {
  it('lets only an authenticated operator act', () => {
    expect(opsAccess(session('operator'), 'api').mayAct).toBe(true);
    expect(opsAccess(session('editor'), 'api').mayAct).toBe(false);
    expect(opsAccess(session('reader'), 'api').mayAct).toBe(false);
    expect(opsAccess(session('operator', false), 'api').mayAct).toBe(false);
  });

  it('does not let an editor act, in either direction', () => {
    // An editor is not an operator with fewer permissions, and an operator
    // is not an editor with more.
    expect(opsAccess(session('editor'), 'api')).toMatchObject({
      mayAct: false,
      mayRead: false,
      blocked: true,
    });
  });

  it('shows a non-operator nothing when a real API is answering', () => {
    expect(opsAccess(session('reader'), 'api').mayRead).toBe(false);
  });

  it('renders the preview for anybody, because there is nothing to leak', () => {
    const access = opsAccess(session('reader', false), 'fixture');
    expect(access.mayRead).toBe(true);
    // …and still refuses to let them decide, and does not claim the role is
    // what is missing, because in a preview there is no role to have.
    expect(access.mayAct).toBe(false);
    expect(access.blocked).toBe(false);
  });

  it('lets a real operator act even against fixtures', () => {
    expect(opsAccess(session('operator'), 'fixture').mayAct).toBe(true);
  });
});
