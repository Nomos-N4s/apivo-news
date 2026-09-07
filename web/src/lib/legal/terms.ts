import { cashbackStrings } from '../../i18n/cashback';
import type { ReadingLanguage } from '../reader/axes';

/**
 * The cashback terms — what a member accepts when they opt in (issue #589).
 *
 * THE RULE THIS MODULE OBEYS, and it is the same one `lib/faq/strings.ts`
 * obeys for the same reason: **it owns headings, not answers.** Every clause
 * body that describes how the product behaves is a reference into
 * `i18n/cashback.ts` — the catalogue that already renders that sentence on a
 * screen. Terms that describe the product differently from the product are
 * worse than no terms at all, and the only way two texts cannot disagree is
 * for there to be one text.
 *
 * What it does own are the seven statements no screen makes: that a credit
 * can be held out of every balance while somebody reviews it; that money
 * already paid is never clawed back; that a minimum exists; which currency
 * governs; what a balance IS; what leaving does; and how amounts are held.
 * Each is checkable against the code, and the pull request that introduced
 * them cites where. `terms.test.ts` counts them, so an eighth has to be a
 * deliberate edit rather than a slow slide back into writing our own copy.
 *
 * WHAT THIS IS NOT. It is not counsel-approved legal drafting, and nothing
 * here should be mistaken for it. It is a plain statement of rules the
 * implementation actually enforces, written so that a member opting in is
 * told the truth rather than nothing. Replacing it with reviewed text is a
 * matter of rewriting these strings; the structure and the version check
 * around them are what make that replacement safe.
 *
 * NO MONEY AND NO ADDRESSES. The withdrawal minimum is a deployment's own
 * figure and the support address is `brand.support.*`; a clause naming
 * either would be wrong for every deployment but one. `terms.test.ts`
 * refuses both, the way `faq/strings.test.ts` and `legal/strings.test.ts`
 * refuse them for their own catalogues.
 */

/**
 * The terms version this text was written against.
 *
 * The brand file carries a version and no text — `Document` is `{ id,
 * version }` — so the words live here and the number lives on the
 * deployment, and the two can drift the moment somebody bumps one. When
 * they disagree, the opt-in refuses to collect consent and says why: a
 * member must never accept a version whose text they were not shown, which
 * is the same thing `POST /participation` refusing a stale version protects
 * against, one layer down.
 */
export const TERMS_TEXT_VERSION = '0.1.0';

/** One numbered promise, with its paragraphs and any list it ends on. */
export interface TermsClause {
  readonly heading: string;
  /** One paragraph per element. References wherever a screen already says it. */
  readonly body: readonly string[];
  /** Rendered as a list under the paragraphs. Empty where there is none. */
  readonly points: readonly string[];
}

/** Everything the terms page renders. */
export interface TermsStrings {
  readonly heading: string;
  readonly intro: string;
  /** Names the version these words were written for. */
  readonly versionLine: (version: string) => string;
  /** Shown when the deployment names no brand of its own. */
  readonly fixtureTitle: string;
  readonly fixtureBody: string;
  /** Shown when this text and the deployment disagree about the version. */
  readonly driftTitle: string;
  readonly driftBody: (textVersion: string, deploymentVersion: string) => string;
  readonly clauses: readonly TermsClause[];
}

/** The half this module owns: headings, and the statements no screen makes. */
interface TermsOwnCopy {
  readonly heading: string;
  readonly intro: string;
  readonly versionLine: (version: string) => string;
  readonly fixtureTitle: string;
  readonly fixtureBody: string;
  readonly driftTitle: string;
  readonly driftBody: (textVersion: string, deploymentVersion: string) => string;
  readonly headings: {
    readonly earning: string;
    readonly rate: string;
    readonly confirming: string;
    readonly holding: string;
    readonly voiding: string;
    readonly reserving: string;
    readonly paying: string;
    readonly afterPayout: string;
    readonly minimum: string;
    readonly currency: string;
    readonly balance: string;
    readonly leaving: string;
    readonly amounts: string;
  };
  /** The bodies no screen says. Each one is checkable against the code. */
  readonly holding: string;
  readonly afterPayout: string;
  readonly minimum: string;
  readonly currency: string;
  readonly balance: string;
  readonly leaving: string;
  readonly amounts: string;
}

const EL: TermsOwnCopy = {
  heading: 'Όροι για την επιστροφή χρημάτων',
  intro:
    'Τι ισχύει όταν συμμετέχεις. Κάθε όρος εδώ περιγράφει κάτι που το σύστημα όντως κάνει, με τα ίδια λόγια που το λένε και οι οθόνες.',
  versionLine: (version) => `Αυτοί οι όροι είναι η έκδοση ${version}.`,
  fixtureTitle: 'Δείγμα',
  fixtureBody:
    'Αυτή η εγκατάσταση δεν ορίζει δική της ταυτότητα, οπότε η έκδοση που αναφέρεται εδώ προέρχεται από ένα δείγμα και δεν δεσμεύει κανέναν.',
  driftTitle: 'Οι εκδόσεις δεν συμφωνούν',
  driftBody: (textVersion, deploymentVersion) =>
    `Το κείμενο εδώ γράφτηκε για την έκδοση ${textVersion}, ενώ αυτή η εγκατάσταση δηλώνει την έκδοση ${deploymentVersion}. Μέχρι να συμφωνήσουν, δεν ζητάμε από κανέναν να τους αποδεχθεί.`,
  headings: {
    earning: 'Πώς προκύπτει μια επιστροφή',
    rate: 'Ποιο ποσοστό ισχύει',
    confirming: 'Πότε επιβεβαιώνεται',
    holding: 'Αν μια πίστωση μπει σε έλεγχο',
    voiding: 'Τι μπορεί να την ακυρώσει',
    reserving: 'Τι δεσμεύεται για μια ανάληψη',
    paying: 'Πώς πληρώνεται',
    afterPayout: 'Μετά την πληρωμή',
    minimum: 'Ελάχιστο ποσό ανάληψης',
    currency: 'Σε ποιο νόμισμα',
    balance: 'Τι είναι το υπόλοιπό σου',
    leaving: 'Αν αποχωρήσεις',
    amounts: 'Πώς τηρούνται τα ποσά',
  },
  holding:
    'Μια πίστωση μπορεί να μπει σε έλεγχο πριν γίνει εκκρεμής. Εμφανίζεται στη λίστα σου σημειωμένη ως τέτοια, δεν προσμετράται σε κανένα υπόλοιπο, και ξεμπλοκάρεται μόνο από ονοματισμένο άτομο με καταγεγραμμένη αιτία.',
  afterPayout:
    'Ό,τι έχει πληρωθεί μένει πληρωμένο. Αν ένα δίκτυο ανακαλέσει μια προμήθεια αφού σου έχει καταβληθεί, η ζημιά βαρύνει εμάς και δεν ζητείται πίσω από εσένα.',
  currency:
    'Πιστώνεσαι σε ένα νόμισμα: αυτό που καταγράφηκε όταν αποδέχθηκες τους όρους. Είναι και το μόνο στο οποίο μπορείς να πληρωθείς· αναφορά σε άλλο νόμισμα πηγαίνει σε έλεγχο αντί να πιστωθεί.',
  minimum:
    'Για κάθε ανάληψη ισχύει ένα ελάχιστο ποσό. Το πορτοφόλι σου δείχνει ποιο είναι και πόσο λείπει ακόμη, γιατί το όριο ανήκει στην εγκατάσταση και όχι σε αυτό το κείμενο.',
  balance:
    'Το υπόλοιπό σου είναι απαίτηση για μελλοντική επιστροφή χρημάτων, όχι χρήματα που κρατάμε για λογαριασμό σου. Δεν μεταφέρεται σε άλλο άτομο και δεν ξοδεύεται εδώ· πληρώνεται στον δικό σου επαληθευμένο λογαριασμό.',
  leaving:
    'Μπορείς να αποχωρήσεις όποτε θέλεις. Σταματά η καταγραφή νέων κλικ, οι καταχωρίσεις που εκκρεμούν συνεχίζουν να εξελίσσονται, και τα οικονομικά αρχεία παραμένουν — δεν διαγράφονται, γιατί είναι και δικά σου.',
  amounts:
    'Κάθε ποσό τηρείται σε ακέραιες υποδιαιρέσεις του νομίσματος, μαζί με ρητό νόμισμα. Κλάσματα λεπτού δεν υπάρχουν πουθενά.',
};

const DE: TermsOwnCopy = {
  heading: 'Bedingungen für das Cashback',
  intro:
    'Was gilt, wenn du teilnimmst. Jede Bedingung hier beschreibt etwas, das das System tatsächlich tut — mit denselben Worten, die auch auf den Bildschirmen stehen.',
  fixtureTitle: 'Beispiel',
  versionLine: (version) => `Diese Bedingungen sind Fassung ${version}.`,
  fixtureBody:
    'Diese Installation benennt keine eigene Identität, deshalb stammt die hier genannte Fassung aus einem Beispiel und bindet niemanden.',
  driftTitle: 'Die Fassungen stimmen nicht überein',
  driftBody: (textVersion, deploymentVersion) =>
    `Der Text hier wurde für Fassung ${textVersion} geschrieben, diese Installation nennt aber Fassung ${deploymentVersion}. Solange das so ist, bitten wir niemanden, ihn anzunehmen.`,
  headings: {
    earning: 'Wie eine Gutschrift entsteht',
    rate: 'Welcher Satz gilt',
    confirming: 'Wann bestätigt wird',
    holding: 'Wenn eine Gutschrift geprüft wird',
    voiding: 'Was sie hinfällig machen kann',
    reserving: 'Was für eine Auszahlung reserviert wird',
    paying: 'Wie ausgezahlt wird',
    afterPayout: 'Nach der Auszahlung',
    minimum: 'Mindestbetrag für eine Auszahlung',
    currency: 'In welcher Währung',
    balance: 'Was dein Guthaben ist',
    leaving: 'Wenn du aussteigst',
    amounts: 'Wie Beträge geführt werden',
  },
  holding:
    'Eine Gutschrift kann vor der Buchung in Prüfung gehen. Sie erscheint in deiner Liste entsprechend gekennzeichnet, zählt zu keinem Guthaben, und nur eine namentlich genannte Person gibt sie frei — mit festgehaltenem Grund.',
  afterPayout:
    'Was ausgezahlt ist, bleibt ausgezahlt. Zieht ein Netzwerk eine Provision zurück, nachdem sie bei dir angekommen ist, tragen wir den Verlust und fordern nichts von dir zurück.',
  currency:
    'Gutgeschrieben wird dir in einer Währung: der, die bei deiner Zustimmung festgehalten wurde. Nur in ihr kannst du auch ausgezahlt werden; eine Meldung in einer anderen Währung geht in Prüfung, statt gutgeschrieben zu werden.',
  minimum:
    'Für jede Auszahlung gilt ein Mindestbetrag. Deine Übersicht zeigt, wie hoch er ist und wie viel noch fehlt — die Grenze gehört der Installation und nicht diesem Text.',
  balance:
    'Dein Guthaben ist ein Anspruch auf eine künftige Rückvergütung, kein Geld, das wir für dich verwahren. Es lässt sich nicht an andere übertragen und nicht hier ausgeben; ausgezahlt wird es auf dein eigenes verifiziertes Konto.',
  leaving:
    'Du kannst jederzeit aussteigen. Neue Klicks werden dann nicht mehr erfasst, offene Buchungen laufen weiter, und die Finanzunterlagen bleiben bestehen — gelöscht wird nichts, denn sie gehören auch dir.',
  amounts:
    'Jeder Betrag wird in ganzen Untereinheiten der Währung geführt, zusammen mit einer ausdrücklichen Währungsangabe. Bruchteile eines Cents gibt es nirgends.',
};

const OWN: Readonly<Record<ReadingLanguage, TermsOwnCopy>> = { el: EL, de: DE };

/**
 * The terms for a reading language: our headings, the product's own sentences.
 *
 * Assembled at call time rather than written out twice, so a change to what a
 * screen says reaches this page in the same commit.
 */
export function termsStrings(lang: ReadingLanguage): TermsStrings {
  const c = cashbackStrings(lang);
  const own = OWN[lang];
  const h = own.headings;

  return {
    heading: own.heading,
    intro: own.intro,
    versionLine: own.versionLine,
    fixtureTitle: own.fixtureTitle,
    fixtureBody: own.fixtureBody,
    driftTitle: own.driftTitle,
    driftBody: own.driftBody,
    clauses: [
      { heading: h.earning, body: [], points: [c.tracking1, c.tracking2, c.tracking3] },
      { heading: h.rate, body: [c.rateAtClick], points: [] },
      { heading: h.confirming, body: [c.pendingExplained], points: [] },
      { heading: h.holding, body: [own.holding], points: [] },
      { heading: h.voiding, body: [], points: [c.void1, c.void2, c.void3] },
      { heading: h.reserving, body: [c.reservedExplained, c.wholeEntriesRule], points: [] },
      {
        heading: h.paying,
        body: [c.withdrawIntro, c.noVerifiedDestination],
        points: [c.approvalStep1, c.approvalStep2, c.approvalStep3],
      },
      { heading: h.afterPayout, body: [own.afterPayout], points: [] },
      { heading: h.minimum, body: [own.minimum], points: [] },
      { heading: h.currency, body: [own.currency], points: [] },
      { heading: h.balance, body: [own.balance], points: [] },
      { heading: h.leaving, body: [own.leaving], points: [] },
      { heading: h.amounts, body: [own.amounts], points: [] },
    ],
  };
}
