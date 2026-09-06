import { isReadingLanguage, READING_LANGUAGES, type ReadingLanguage } from '../reader/axes';

/**
 * Which language an operator screen renders in.
 *
 * The operator queues carry no place and no language in their paths — the
 * member's two axes are about what a member reads, and an operator works one
 * queue rather than one locale. But the copy still has to come from a
 * catalogue keyed by language, so the choice is made here rather than by a
 * constant buried in each page.
 *
 * `?lang=` decides it, and the first mounted reading language is the
 * default. That is a deliberate placeholder rather than a product decision:
 * whose language an internal surface renders in is a question for whoever
 * staffs it, and one query parameter is the smallest thing that can be
 * changed once it is answered.
 */
export const DEFAULT_OPS_LANGUAGE: ReadingLanguage = READING_LANGUAGES[0];

/** The operator language a URL asks for. */
export function opsLanguage(url: URL): ReadingLanguage {
  const asked = url.searchParams.get('lang');
  return asked !== null && isReadingLanguage(asked) ? asked : DEFAULT_OPS_LANGUAGE;
}
