import { describe, expect, it } from 'vitest';

import { BASELINE_FIELD, parseSourceEditForm, renderedBaseline } from './sourceForm';

const RENDERED = {
  name: 'Münchner Tagblatt',
  url: 'https://example.test/feed.xml',
  licence_terms: 'Extract and link permitted.',
  active: true,
};

/**
 * One submission of the row edit form. `active` is spelled the way a
 * browser does it: present as "on" when ticked, absent entirely when not.
 */
function editForm(
  fields: Partial<typeof RENDERED> & { readonly baseline?: string | null } = {},
): FormData {
  const values = { ...RENDERED, ...fields };
  const form = new FormData();
  form.append('intent', 'update');
  form.append('id', 'src-1');
  if (fields.baseline !== null) {
    form.append(BASELINE_FIELD, fields.baseline ?? renderedBaseline(RENDERED));
  }
  form.append('name', values.name);
  form.append('url', values.url);
  form.append('licence_terms', values.licence_terms);
  if (values.active) {
    form.append('active', 'on');
  }
  return form;
}

describe('parseSourceEditForm', () => {
  it('leaves an untouched field out of the patch', () => {
    // The whole point: the form pre-fills all four, so a save that
    // changed one must not carry the other three. The API diffs supplied
    // values against the CURRENT row, so re-sending an untouched url
    // would revert whatever another editor set meanwhile — and the
    // source.updated event would blame this editor for it.
    const result = parseSourceEditForm(editForm({ name: 'Münchner Tagblatt (Kultur)' }));
    if (!result.ok) {
      throw new Error(`expected a patch, got refusal ${result.refusal}`);
    }
    expect(result.patch).toEqual({ name: 'Münchner Tagblatt (Kultur)' });
    expect(result.patch).not.toHaveProperty('url');
    expect(result.patch).not.toHaveProperty('licence_terms');
    expect(result.patch).not.toHaveProperty('active');
    expect(result.id).toBe('src-1');
  });

  it('carries a changed field, and only it, for each of the four', () => {
    const cases = [
      { field: 'name', form: editForm({ name: 'Renamed' }), patch: { name: 'Renamed' } },
      {
        field: 'url',
        form: editForm({ url: 'https://example.test/feed/moved.xml' }),
        patch: { url: 'https://example.test/feed/moved.xml' },
      },
      {
        field: 'licence_terms',
        form: editForm({ licence_terms: 'Renegotiated terms.' }),
        patch: { licence_terms: 'Renegotiated terms.' },
      },
      // Unticking the box submits no `active` at all; the absence IS the
      // deactivation, and it must reach the patch as false.
      { field: 'active', form: editForm({ active: false }), patch: { active: false } },
    ];
    for (const { field, form, patch } of cases) {
      const result = parseSourceEditForm(form);
      if (!result.ok) {
        throw new Error(`expected a patch for ${field}, got refusal ${result.refusal}`);
      }
      expect(result.patch).toEqual(patch);
    }
  });

  it('sends every field the editor actually changed', () => {
    const result = parseSourceEditForm(
      editForm({ name: 'Renamed', licence_terms: 'Renegotiated terms.', active: false }),
    );
    if (!result.ok) {
      throw new Error(`expected a patch, got refusal ${result.refusal}`);
    }
    expect(result.patch).toEqual({
      name: 'Renamed',
      licence_terms: 'Renegotiated terms.',
      active: false,
    });
  });

  it('refuses a submission that changed nothing', () => {
    // Not a failure and not a save: nothing is sent, and the screen says
    // so rather than confirming an edit that never happened.
    const result = parseSourceEditForm(editForm());
    expect(result.ok).toBe(false);
    if (result.ok) {
      return;
    }
    expect(result.refusal).toBe('no-change');
    expect(result.id).toBe('src-1');
    expect(result.typed).toEqual(RENDERED);
  });

  it('compares the url trimmed, as the API stores it', () => {
    const result = parseSourceEditForm(editForm({ url: `  ${RENDERED.url}  ` }));
    expect(result.ok).toBe(false);
  });

  it('does not read a textarea CRLF as an edit', () => {
    // Browsers submit textarea content with CRLF while the row holds LF;
    // a line ending nobody typed must not reach the audit stream.
    const multiline = { ...RENDERED, licence_terms: 'Extract and link.\nAttribution required.' };
    const form = new FormData();
    form.append('id', 'src-1');
    form.append(BASELINE_FIELD, renderedBaseline(multiline));
    form.append('name', multiline.name);
    form.append('url', multiline.url);
    form.append('licence_terms', 'Extract and link.\r\nAttribution required.');
    form.append('active', 'on');
    const result = parseSourceEditForm(form);
    expect(result.ok).toBe(false);
    if (!result.ok) {
      expect(result.refusal).toBe('no-change');
    }
  });

  it('keeps what was typed through every refusal', () => {
    const typed = {
      name: 'Half-typed name',
      url: 'https://example.test/feed/new.xml',
      licence_terms: 'Half-typed terms.',
      active: false,
    };
    const result = parseSourceEditForm(editForm({ ...typed, baseline: null }));
    expect(result.ok).toBe(false);
    if (result.ok) {
      return;
    }
    expect(result.typed).toEqual(typed);
  });

  it('sends nothing when the form did not carry what it showed', () => {
    // Without the baseline, changed and untouched cannot be told apart,
    // and the one alternative — send all four — is the silent clobber
    // this parser exists to prevent.
    for (const baseline of ['', 'not json', '{"name":"only"}', 'null']) {
      const result = parseSourceEditForm(editForm({ baseline, name: 'Renamed' }));
      expect(result.ok).toBe(false);
      if (!result.ok) {
        expect(result.refusal).toBe('no-baseline');
      }
    }
  });

  it('refuses a submission naming no source', () => {
    const form = editForm();
    form.set('id', '');
    const result = parseSourceEditForm(form);
    expect(result.ok).toBe(false);
    if (!result.ok) {
      expect(result.refusal).toBe('no-source');
      expect(result.id).toBeNull();
    }
  });
});

describe('renderedBaseline', () => {
  it('survives multi-line licence terms', () => {
    // The hidden input's value sanitiser strips newlines, so the baseline
    // is JSON: a baseline that lost them would differ from every
    // submission and make an untouched field look edited on every save.
    const terms = 'Extract and link.\nAttribution required.';
    const encoded = renderedBaseline({ ...RENDERED, licence_terms: terms });
    expect(encoded).not.toContain('\n');
    expect(JSON.parse(encoded)).toEqual({ ...RENDERED, licence_terms: terms });
  });
});
