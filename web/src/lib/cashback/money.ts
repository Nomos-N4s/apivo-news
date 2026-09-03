import type { ReadingLanguage } from '../reader/axes';

/**
 * Money on the member's screen.
 *
 * Constitution C-6: every monetary amount is integer minor units with an
 * explicit ISO-4217 currency, and no decimal ever crosses an API boundary.
 * The cashback API therefore emits `{ minor, currency }` everywhere and
 * says, in as many words, that "formatting for display happens in the
 * frontend from these two fields". This module is that formatting, and it
 * is the only place in the frontend that turns a count of cents into
 * something a person reads.
 *
 * **There is no float path here.** The obvious implementation divides the
 * minor units by a hundred and hands the result to `Intl`, which works for
 * every amount this product will ever hold and is still the wrong habit to
 * establish in the one module whose entire job is an invariant about not
 * doing that. `Intl.NumberFormat#format` accepts a decimal *string*
 * (ECMA-402 NumberFormat v3, in Node 22), so the decimal is assembled from
 * the integer by slicing digits and never becomes a `number` at all.
 */

/** An amount as the API emits it. */
export interface Money {
  /** Integer minor units. Negative for a debit or a reversal. */
  readonly minor: number;
  /** ISO-4217, uppercase. */
  readonly currency: string;
}

/** Reports whether an unknown value from the wire is a money object. */
export function isMoney(value: unknown): value is Money {
  if (typeof value !== 'object' || value === null) {
    return false;
  }
  const record = value as Record<string, unknown>;
  return (
    typeof record['minor'] === 'number' &&
    Number.isInteger(record['minor']) &&
    typeof record['currency'] === 'string' &&
    /^[A-Z]{3}$/.test(record['currency'])
  );
}

/** Zero in a currency — an empty wallet still has a currency. */
export function zero(currency: string): Money {
  return { minor: 0, currency };
}

/**
 * Adds amounts that share a currency.
 *
 * A mixed-currency sum throws rather than picking one: the ledger rejects a
 * transfer whose postings do not sum to zero *per currency* (C-1), and a
 * total that silently added euros to something else would be the frontend
 * disagreeing with the ledger about what a member is owed.
 */
export function sum(amounts: readonly Money[]): Money {
  if (amounts.length === 0) {
    throw new RangeError('sum of no amounts has no currency');
  }
  const [first, ...rest] = amounts as [Money, ...Money[]];
  let total = first.minor;
  for (const amount of rest) {
    if (amount.currency !== first.currency) {
      throw new RangeError(
        `cannot add ${amount.currency} to ${first.currency}: a total spanning currencies is not an amount`,
      );
    }
    total += amount.minor;
  }
  return { minor: total, currency: first.currency };
}

/** How many decimal places a currency is written with, in this language. */
function fractionDigits(lang: ReadingLanguage, currency: string): number {
  return (
    new Intl.NumberFormat(lang, { style: 'currency', currency }).resolvedOptions()
      .maximumFractionDigits ?? 2
  );
}

/**
 * The minor units as a decimal string — `-145` and 2 digits becomes
 * `"-1.45"`. Pure integer and string work; the result is what `Intl` is
 * given.
 */
export function decimalString(minor: number, digits: number): string {
  const sign = minor < 0 ? '-' : '';
  const magnitude = Math.abs(minor).toString().padStart(digits + 1, '0');
  if (digits === 0) {
    return `${sign}${magnitude}`;
  }
  const whole = magnitude.slice(0, magnitude.length - digits);
  const fraction = magnitude.slice(magnitude.length - digits);
  return `${sign}${whole}.${fraction}`;
}

/**
 * The decimal, narrowed to the type `Intl.NumberFormat#format` declares.
 *
 * TypeScript types that parameter as `Intl.StringNumericLiteral` — the
 * template-literal type `${number}` — which no computed string can satisfy
 * however well formed it is. The assertion is confined to this one function
 * so the two call sites stay honest, and what it asserts is exactly what
 * `decimalString` produces: an optional minus, digits, and at most one
 * point. `money.test.ts` holds that over its whole range, and the runtime
 * has always accepted the string; only the type has not.
 */
function asNumericLiteral(decimal: string): Intl.StringNumericLiteral {
  return decimal as Intl.StringNumericLiteral;
}

/** An amount for display — el "1,45 €", de "1,45 €". */
export function formatMoney(lang: ReadingLanguage, amount: Money): string {
  const digits = fractionDigits(lang, amount.currency);
  return new Intl.NumberFormat(lang, {
    style: 'currency',
    currency: amount.currency,
  }).format(asNumericLiteral(decimalString(amount.minor, digits)));
}

/**
 * A credit with its sign made explicit — "+1,45 €" — for entry rows, where
 * the difference between money arriving and money going back matters more
 * than the figure does.
 *
 * Zero gets no sign. A reversal is negative and reads as such.
 */
export function formatSignedMoney(lang: ReadingLanguage, amount: Money): string {
  const formatted = formatMoney(lang, amount);
  return amount.minor > 0 ? `+${formatted}` : formatted;
}

/**
 * A rate band as the member earns it — basis points to a percentage.
 *
 * The API publishes `bps` because a rate is stored as an integer for the
 * same reason money is. 700 bps is 7%, and 725 is 7.25%: the trailing
 * zeroes are dropped rather than printing "7,00 %" beside "7,25 %" in the
 * same column.
 */
export function formatRateBps(lang: ReadingLanguage, bps: number): string {
  return new Intl.NumberFormat(lang, {
    style: 'percent',
    maximumFractionDigits: 2,
  }).format(asNumericLiteral(decimalString(bps, 4)));
}
