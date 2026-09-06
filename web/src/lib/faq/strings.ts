import { cashbackStrings } from '../../i18n/cashback';
import type { ReadingLanguage } from '../reader/axes';
import { uiStrings } from '../reader/strings';

/**
 * The FAQ (issue #528).
 *
 * This module owns QUESTIONS and STRUCTURE. It owns no answer.
 *
 * The design board states the rule twice — once to the reader, once as a
 * comment to whoever builds it: "Every answer below is copy that already
 * exists in i18n/cashback.ts. A FAQ that paraphrases the product is a
 * second source of truth." So every answer here is a member reference into
 * a catalogue that already renders that sentence on a screen. Amend the
 * wallet's wording and the FAQ moves in the same commit; rename a key and
 * `astro check` fails rather than the page going quietly stale.
 *
 * A re-export would be no better than a literal: it is still a second place
 * the sentence can be edited. `strings.test.ts` asserts by SET MEMBERSHIP
 * that every rendered answer is a string some catalogue already ships, so
 * the moment somebody hand-writes one here — or "tidies" a trailing space —
 * the suite goes red instead of the two surfaces drifting apart.
 *
 * Two of the answers below, `approvalStep1` and `approvalStep3`, have been
 * carried in both language catalogues while being referenced by nothing at
 * all. This is the surface they were written for.
 *
 * Nothing here is money. The withdrawal minimum is a live figure and belongs
 * on the screens that know the member's balance (`belowThreshold`, rendered
 * by the wallet and the withdrawal page). A threshold frozen into a
 * translated question is exactly the drift this module exists to prevent
 * (FR-071), which is why the board's "I asked for 10,00 €" becomes "I asked
 * for an amount".
 */

/** One question and the answer paragraphs it renders, in order. */
export interface FaqEntry {
  readonly question: string;
  /** One paragraph per element. Always references, never literals. */
  readonly answer: readonly string[];
}

/** A titled group of entries — the board draws two. */
export interface FaqSection {
  readonly heading: string;
  readonly entries: readonly FaqEntry[];
}

/** Everything the FAQ page renders. */
export interface FaqStrings {
  readonly heading: string;
  readonly intro: string;
  readonly noAnswerHere: string;
  readonly contact: string;
  readonly sections: readonly FaqSection[];
}

type QuestionKey =
  | 'pending'
  | 'notAllCounts'
  | 'moreHeld'
  | 'rateChanged'
  | 'howLong'
  | 'extractOnly'
  | 'whoApproves'
  | 'languageAndPlace';

/** The only copy this module owns: the questions, and the page's own frame. */
interface FaqOwnCopy {
  readonly intro: string;
  readonly noAnswerHere: string;
  readonly questions: Readonly<Record<QuestionKey, string>>;
}

const EL: FaqOwnCopy = {
  intro:
    'Οι απαντήσεις είναι οι ίδιες προτάσεις που ήδη εμφανίζονται στις οθόνες. Αν εδώ λέγαμε κάτι διαφορετικό, η μία από τις δύο θα ήταν λάθος.',
  noAnswerHere: 'Δεν υπάρχει εδώ η απάντηση;',
  questions: {
    pending: 'Γιατί είναι «σε εκκρεμότητα» η καταχώρισή μου;',
    notAllCounts: 'Γιατί δεν μετράνε όλα τα ποσά μου στην ανάληψη;',
    moreHeld: 'Ζήτησα ένα ποσό και δεσμεύτηκε μεγαλύτερο. Γιατί;',
    rateChanged: 'Άλλαξε το ποσοστό μετά την αγορά μου.',
    howLong: 'Πόσο θα πάρει η πληρωμή;',
    extractOnly: 'Γιατί βλέπω μόνο ένα απόσπασμα;',
    whoApproves: 'Ποιος αποφασίζει τι δημοσιεύεται;',
    languageAndPlace: 'Διαβάζω ελληνικά αλλά ζω στο Μόναχο.',
  },
};

const DE: FaqOwnCopy = {
  intro:
    'Die Antworten hier sind dieselben Sätze, die auch auf den Bildschirmen stehen. Stünde hier etwas anderes, wäre eine der beiden Stellen falsch.',
  noAnswerHere: 'Steht die Antwort hier nicht?',
  questions: {
    pending: 'Warum ist meine Buchung „offen“?',
    notAllCounts: 'Warum zählen nicht alle meine Beträge für die Auszahlung?',
    moreHeld: 'Ich habe einen Betrag angefordert, reserviert wurde mehr. Warum?',
    rateChanged: 'Der Satz hat sich nach meinem Kauf geändert.',
    howLong: 'Wie lange dauert die Auszahlung?',
    extractOnly: 'Warum sehe ich nur einen Auszug?',
    whoApproves: 'Wer entscheidet, was veröffentlicht wird?',
    languageAndPlace: 'Ich lese auf Griechisch, wohne aber in München.',
  },
};

const OWN: Readonly<Record<ReadingLanguage, FaqOwnCopy>> = { el: EL, de: DE };

/** The FAQ for a reading language: our questions, the product's answers. */
export function faqStrings(lang: ReadingLanguage): FaqStrings {
  const c = cashbackStrings(lang);
  const u = uiStrings(lang);
  const own = OWN[lang];
  const q = own.questions;

  return {
    // Derived, never written: the heading and the footer link that reaches
    // it are one key, so they cannot come to disagree.
    heading: u.faqLabel,
    intro: own.intro,
    noAnswerHere: own.noAnswerHere,
    contact: u.contact,
    sections: [
      {
        heading: c.cashback,
        entries: [
          { question: q.pending, answer: [c.pendingExplained] },
          { question: q.notAllCounts, answer: [c.withdrawIntro] },
          { question: q.moreHeld, answer: [c.wholeEntriesRule] },
          { question: q.rateChanged, answer: [c.rateAtClick] },
          { question: q.howLong, answer: [c.approvalStep1, c.approvalStep2, c.approvalStep3] },
        ],
      },
      {
        heading: c.news,
        entries: [
          { question: q.extractOnly, answer: [u.wholeExtractBody] },
          { question: q.whoApproves, answer: [u.reassurance] },
          { question: q.languageAndPlace, answer: [u.independenceLine] },
        ],
      },
    ],
  };
}
