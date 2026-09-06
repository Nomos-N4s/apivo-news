import type { ReadingLanguage } from '../reader/axes';

/**
 * The cashback URL scheme, built in one place.
 *
 * `/{lang}/{place[+place…]}/cashback/…` — the same two axes the reader
 * pages carry, in the same two segments, for the reason Principle VII
 * gives: a Greek speaker in Munich reads Greek about Munich, and the two
 * facts travel separately. The catalogue endpoint takes `lang` and `place`
 * as separate parameters, so the URL that leads to it does too.
 *
 * `wallet` and `withdraw` are static segments under the same prefix a
 * merchant slug occupies. Astro resolves a static route before a dynamic
 * one, so `/el/munich/cashback/wallet` reaches the wallet even if a
 * retailer is ever slugged `wallet` — and that retailer becomes
 * unreachable rather than the wallet becoming a shop, which is the right
 * way round for the failure to fall.
 */

/** The place segment: slugs joined the way the reader pages join them. */
export function placeSegment(slugs: readonly string[]): string {
  return slugs.join('+');
}

/** The catalogue — the cashback front door. */
export function cataloguePath(lang: ReadingLanguage, slugs: readonly string[]): string {
  return `/${lang}/${placeSegment(slugs)}/cashback`;
}

/** The member's wallet and entry list. */
export function walletPath(lang: ReadingLanguage, slugs: readonly string[]): string {
  return `${cataloguePath(lang, slugs)}/wallet`;
}

/** The withdrawal request and its status. */
export function withdrawPath(lang: ReadingLanguage, slugs: readonly string[]): string {
  return `${cataloguePath(lang, slugs)}/withdraw`;
}

/** One retailer's page. */
export function merchantPath(
  lang: ReadingLanguage,
  slugs: readonly string[],
  merchantSlug: string,
): string {
  return `${cataloguePath(lang, slugs)}/${encodeURIComponent(merchantSlug)}`;
}

/** The operator queues, which carry no axes: an operator works in one place. */
export const OPS_PATHS = {
  unattributed: '/ops/unattributed',
  held: '/ops/held',
  withdrawals: '/ops/withdrawals',
  reconciliation: '/ops/reconciliation',
} as const;
