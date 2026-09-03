import type { ReadingLanguage } from '../lib/reader/axes';
import type { EntryState, WithdrawalState } from '../lib/cashback/types';

/**
 * Member-facing cashback copy, keyed by BCP-47 primary language subtag.
 *
 * Two constitutional rules meet in this file. **Rebrandability**: all
 * member-facing text lives in translation catalogues, and no product name,
 * legal entity, domain, colour, support address or currency default is
 * hardcoded anywhere — so nothing below names the brand, and the brand
 * loader supplies it where a sentence needs one. **Principle VII**: keys
 * are the language axis alone. There is no `el-DE`; a Greek speaker in
 * Munich reads Greek, and where they are is a separate axis the API takes
 * as a separate parameter.
 *
 * The copy is not a translation of the design mockups, which are English
 * placeholders written against a withdrawal flow the contract does not
 * implement. Where the two disagree the contract wins and the sentence is
 * written from it — most visibly around approval, reservation and the
 * payout threshold. The divergences are recorded in issues #442 to #447.
 *
 * It lives under `src/i18n/` rather than beside the client because the
 * cashback surfaces are the first in this frontend to need one catalogue
 * across a member area and an operator area; `lib/reader/strings.ts` and
 * `lib/editorial/strings.ts` each serve one.
 */

/** Every string a cashback surface renders. */
export interface CashbackStrings {
  /* Chrome */
  readonly cashback: string;
  readonly wallet: string;
  readonly catalogue: string;
  readonly backToWallet: string;
  readonly fixtureNotice: string;

  /* Wallet totals */
  readonly confirmedWithdrawable: string;
  readonly pending: string;
  readonly reserved: string;
  readonly paidOut: string;
  readonly payoutThreshold: string;
  readonly reservedExplained: string;
  readonly pendingExplained: string;

  /* Entry list */
  readonly entries: string;
  readonly filterAll: string;
  readonly filterPending: string;
  readonly filterConfirmed: string;
  readonly filterPaid: string;
  readonly entryCount: (shown: number, total: number) => string;
  readonly entryStates: Readonly<Record<EntryState, string>>;
  readonly expectedConfirmation: string;
  readonly sale: string;
  readonly reversalOf: string;
  readonly reversalReason: string;
  readonly noEntries: string;
  readonly noEntriesInFilter: string;
  readonly rateAtClick: string;

  /* Catalogue and merchant */
  readonly searchLabel: string;
  readonly searchPlaceholder: string;
  readonly placeLabel: string;
  readonly placementNotForSale: string;
  readonly shownInLanguage: (language: string) => string;
  readonly rates: string;
  readonly noRatesToday: string;
  readonly conditions: string;
  readonly exclusions: string;
  readonly validUntil: string;
  readonly confirmationTimeUnknown: string;
  readonly shopAndEarn: string;
  readonly seeTerms: string;
  readonly noMerchants: string;
  readonly clickoutFailed: string;
  readonly merchantNotFound: string;

  /* Withdrawal */
  readonly withdraw: string;
  readonly withdrawTitle: string;
  readonly withdrawIntro: string;
  readonly amountLabel: string;
  readonly destinationLabel: string;
  readonly destinationUnverified: string;
  readonly noVerifiedDestination: string;
  readonly whatHappensNext: string;
  readonly approvalStep1: string;
  readonly approvalStep2: string;
  readonly approvalStep3: string;
  readonly wholeEntriesRule: string;
  readonly belowThreshold: (threshold: string) => string;
  readonly shortfall: (amount: string) => string;
  readonly requestRecorded: (reference: string) => string;
  readonly reservedForRequest: (amount: string) => string;
  readonly withdrawalStates: Readonly<Record<WithdrawalState, string>>;
  readonly decisionReason: string;
  readonly payoutReference: string;
  readonly noWithdrawals: string;

  /* Participation */
  readonly notParticipating: string;
  readonly optIn: string;

  /* Operator */
  readonly operations: string;
  readonly queueUnattributed: string;
  readonly queueHeld: string;
  readonly queueWithdrawals: string;
  readonly queueReconciliation: string;
  readonly oldestFirst: string;
  readonly reasonLabel: string;
  readonly reasonRequired: string;
  readonly decisionIsPermanent: string;
  readonly attribute: string;
  readonly dismiss: string;
  readonly notAttributable: string;
  readonly release: string;
  readonly reject: string;
  readonly approve: string;
  readonly heldSince: string;
  readonly holdRule: string;
  readonly networkReported: string;
  readonly emptyQueue: string;
  readonly differenceKinds: Readonly<Record<string, string>>;
  readonly expected: string;
  readonly actual: string;
  readonly delta: string;
  readonly superseded: string;
  readonly resolutionExplained: string;
  readonly resolutionAbsorbed: string;
  readonly openDifferenceIsTheChase: string;
}

const el: CashbackStrings = {
  cashback: 'Επιστροφή χρημάτων',
  wallet: 'Το πορτοφόλι σου',
  catalogue: 'Καταστήματα',
  backToWallet: 'Πίσω στο πορτοφόλι',
  fixtureNotice:
    'Δείγμα δεδομένων. Τα ποσά σε αυτή τη σελίδα δεν είναι πραγματικά και δεν αντιστοιχούν σε καμία αγορά.',

  confirmedWithdrawable: 'Επιβεβαιωμένα, διαθέσιμα για ανάληψη',
  pending: 'Σε εκκρεμότητα',
  reserved: 'Δεσμευμένα',
  paidOut: 'Έχουν πληρωθεί',
  payoutThreshold: 'Ελάχιστο ποσό ανάληψης',
  reservedExplained:
    'Δεσμευμένα για αίτημα ανάληψης που δεν έχει ολοκληρωθεί ακόμη. Δεν μπορούν να ζητηθούν δεύτερη φορά.',
  pendingExplained:
    'Επιβεβαιώνονται με το χρονοδιάγραμμα του συνεργάτη, όχι με το δικό μας.',

  entries: 'Καταχωρίσεις',
  filterAll: 'Όλες',
  filterPending: 'Σε εκκρεμότητα',
  filterConfirmed: 'Επιβεβαιωμένες',
  filterPaid: 'Πληρωμένες',
  entryCount: (shown, total) =>
    shown === total ? `${shown} καταχωρίσεις` : `${shown} από ${total} καταχωρίσεις`,
  entryStates: {
    pending: 'Σε εκκρεμότητα',
    confirmed: 'Επιβεβαιωμένη',
    paid: 'Πληρωμένη',
    held: 'Σε έλεγχο',
    reversed: 'Ακυρωμένη',
    declined: 'Απορρίφθηκε',
  },
  expectedConfirmation: 'Αναμένεται επιβεβαίωση',
  sale: 'Αγορά',
  reversalOf: 'Ακύρωση της καταχώρισης',
  reversalReason: 'Αιτία',
  noEntries:
    'Καμία καταχώριση ακόμη. Ξεκίνησε από τα καταστήματα — η επιστροφή καταγράφεται μετά την αγορά.',
  noEntriesInFilter: 'Καμία καταχώριση σε αυτή την κατάσταση.',
  rateAtClick:
    'Ισχύει το ποσοστό που εμφανιζόταν όταν πάτησες τον σύνδεσμο. Μια μεταγενέστερη αλλαγή δεν αγγίζει αυτή την καταχώριση.',

  searchLabel: 'Αναζήτηση καταστημάτων',
  searchPlaceholder: 'π.χ. τρόφιμα, φαρμακείο',
  placeLabel: 'Τόπος',
  placementNotForSale:
    'Η σειρά προκύπτει από τη γλώσσα και τους τόπους σου. Καμία θέση δεν πωλείται.',
  shownInLanguage: (language) => `Εμφανίζεται στα ${language}`,
  rates: 'Ποσοστά',
  noRatesToday:
    'Αυτό το κατάστημα δεν έχει ενεργό ποσοστό αυτή τη στιγμή.',
  conditions: 'Προϋποθέσεις',
  exclusions: 'Εξαιρέσεις',
  validUntil: 'Ισχύει έως',
  confirmationTimeUnknown:
    'Ο χρόνος επιβεβαίωσης δεν είναι καταγεγραμμένος για αυτό το κατάστημα, οπότε δεν τον αναφέρουμε.',
  shopAndEarn: 'Άνοιγμα καταστήματος',
  seeTerms: 'Όροι',
  noMerchants: 'Κανένα κατάστημα για αυτούς τους τόπους.',
  clickoutFailed:
    'Το κλικ δεν καταγράφηκε, οπότε δεν σε στείλαμε στο κατάστημα. Χωρίς καταγραφή η αγορά δεν αποδίδεται σε σένα — δοκίμασε ξανά σε λίγο.',
  merchantNotFound: 'Αυτό το κατάστημα δεν είναι διαθέσιμο.',

  withdraw: 'Ανάληψη',
  withdrawTitle: 'Ανάληψη στον λογαριασμό σου',
  withdrawIntro:
    'Μόνο επιβεβαιωμένα ποσά. Οι εκκρεμείς καταχωρίσεις παραμένουν μέχρι να τις επιβεβαιώσει ο συνεργάτης.',
  amountLabel: 'Ποσό',
  destinationLabel: 'Προορισμός',
  destinationUnverified: 'Δεν έχει επαληθευτεί ακόμη',
  noVerifiedDestination:
    'Δεν υπάρχει επαληθευμένος προορισμός. Η πληρωμή γίνεται μόνο σε λογαριασμό που σου ανήκει και έχει επαληθευτεί.',
  whatHappensNext: 'Τι γίνεται στη συνέχεια',
  approvalStep1: 'Το αίτημα καταγράφεται και το ποσό δεσμεύεται αμέσως.',
  approvalStep2: 'Ένα ονοματισμένο άτομο το εγκρίνει — κάθε ανάληψη, χωρίς εξαίρεση.',
  approvalStep3: 'Μετά την έγκριση η πληρωμή αποστέλλεται και λαμβάνεις αναφορά.',
  wholeEntriesRule:
    'Η δέσμευση γίνεται σε ολόκληρες καταχωρίσεις, από την παλαιότερη. Κάθε καταχώριση στηρίζεται σε μία απόδειξη του δικτύου, οπότε δεν υπάρχει μισή για να δεσμευτεί — το δεσμευμένο ποσό μπορεί να είναι μεγαλύτερο από αυτό που ζήτησες.',
  belowThreshold: (threshold) => `Το ελάχιστο ποσό ανάληψης είναι ${threshold}.`,
  shortfall: (amount) => `Λείπουν ${amount}.`,
  requestRecorded: (reference) => `Το αίτημα καταγράφηκε: ${reference}`,
  reservedForRequest: (amount) => `Δεσμεύτηκαν ${amount}`,
  withdrawalStates: {
    awaiting_approval: 'Αναμένει έγκριση',
    approved: 'Εγκρίθηκε',
    rejected: 'Απορρίφθηκε',
    submitted: 'Στάλθηκε προς πληρωμή',
    settled: 'Πληρώθηκε',
    failed: 'Απέτυχε',
  },
  decisionReason: 'Αιτιολογία',
  payoutReference: 'Αριθμός πληρωμής',
  noWithdrawals: 'Καμία ανάληψη ακόμη.',

  notParticipating: 'Δεν συμμετέχεις ακόμη στην επιστροφή χρημάτων.',
  optIn: 'Συμμετοχή',

  operations: 'Λειτουργίες',
  queueUnattributed: 'Χωρίς αντιστοίχιση',
  queueHeld: 'Σε αναμονή',
  queueWithdrawals: 'Αναλήψεις προς έγκριση',
  queueReconciliation: 'Συμφωνία καταστάσεων',
  oldestFirst: 'παλαιότερα πρώτα',
  reasonLabel: 'Αιτιολογία',
  reasonRequired: 'Η αιτιολογία είναι υποχρεωτική· χωρίς αυτήν η ενέργεια απορρίπτεται.',
  decisionIsPermanent:
    'Η απόφαση καταγράφεται στο όνομά σου και δεν διορθώνεται εκ των υστέρων — ο ίδιος κανόνας με τη σύνταξη.',
  attribute: 'Αντιστοίχιση',
  dismiss: 'Απόρριψη',
  notAttributable:
    'Το δίκτυο ανέφερε παραπομπή που δεν αντιστοιχεί σε κλικ. Επιτρέπεται μόνο απόρριψη.',
  release: 'Αποδέσμευση',
  reject: 'Απόρριψη',
  approve: 'Έγκριση',
  heldSince: 'Σε αναμονή από',
  holdRule: 'Κανόνας',
  networkReported: 'Το δίκτυο ανέφερε',
  emptyQueue: 'Καμία εκκρεμότητα σε αυτή τη λίστα.',
  differenceKinds: {
    reported_not_paid: 'Αναφέρθηκε, δεν πληρώθηκε',
    amount_mismatch: 'Διαφορά ποσού',
    paid_not_reported: 'Πληρώθηκε, δεν αναφέρθηκε',
  },
  expected: 'Αναμενόμενο',
  actual: 'Πραγματικό',
  delta: 'Διαφορά',
  superseded: 'Το δίκτυο αναθεώρησε την αναφορά',
  resolutionExplained: 'Εξηγήθηκε',
  resolutionAbsorbed: 'Απορροφήθηκε',
  openDifferenceIsTheChase:
    'Μια ανοιχτή διαφορά είναι η ίδια η διεκδίκηση και κρατά κλειστή την επιβεβαίωση. Δεν κλείνει επειδή περιμένουμε πληρωμή.',
};

const de: CashbackStrings = {
  cashback: 'Cashback',
  wallet: 'Deine Übersicht',
  catalogue: 'Shops',
  backToWallet: 'Zurück zur Übersicht',
  fixtureNotice:
    'Beispieldaten. Die Beträge auf dieser Seite sind erfunden und gehören zu keinem Einkauf.',

  confirmedWithdrawable: 'Bestätigt und auszahlbar',
  pending: 'Offen',
  reserved: 'Reserviert',
  paidOut: 'Ausgezahlt',
  payoutThreshold: 'Mindestbetrag für eine Auszahlung',
  reservedExplained:
    'Für eine noch nicht abgeschlossene Auszahlung reserviert. Dieser Betrag lässt sich kein zweites Mal anfordern.',
  pendingExplained: 'Bestätigt wird nach dem Zeitplan des Partners, nicht nach unserem.',

  entries: 'Buchungen',
  filterAll: 'Alle',
  filterPending: 'Offen',
  filterConfirmed: 'Bestätigt',
  filterPaid: 'Ausgezahlt',
  entryCount: (shown, total) =>
    shown === total ? `${shown} Buchungen` : `${shown} von ${total} Buchungen`,
  entryStates: {
    pending: 'Offen',
    confirmed: 'Bestätigt',
    paid: 'Ausgezahlt',
    held: 'In Prüfung',
    reversed: 'Storniert',
    declined: 'Abgelehnt',
  },
  expectedConfirmation: 'Bestätigung erwartet',
  sale: 'Einkauf',
  reversalOf: 'Storno zu Buchung',
  reversalReason: 'Grund',
  noEntries:
    'Noch keine Buchungen. Fang bei den Shops an — gutgeschrieben wird nach dem Einkauf.',
  noEntriesInFilter: 'Keine Buchung in diesem Zustand.',
  rateAtClick:
    'Es gilt der Satz, der beim Klick angezeigt wurde. Eine spätere Änderung rührt diese Buchung nicht an.',

  searchLabel: 'Shops durchsuchen',
  searchPlaceholder: 'z. B. Lebensmittel, Apotheke',
  placeLabel: 'Ort',
  placementNotForSale:
    'Die Reihenfolge folgt deiner Sprache und deinen Orten. Kein Platz ist käuflich.',
  shownInLanguage: (language) => `Angezeigt auf ${language}`,
  rates: 'Sätze',
  noRatesToday: 'Dieser Shop hat derzeit keinen aktiven Satz.',
  conditions: 'Bedingungen',
  exclusions: 'Ausgenommen',
  validUntil: 'Gültig bis',
  confirmationTimeUnknown:
    'Für diesen Shop ist keine Bestätigungsdauer hinterlegt, deshalb nennen wir keine.',
  shopAndEarn: 'Zum Shop',
  seeTerms: 'Bedingungen',
  noMerchants: 'Keine Shops für diese Orte.',
  clickoutFailed:
    'Der Klick wurde nicht erfasst, deshalb haben wir dich nicht zum Shop geschickt. Ohne Erfassung lässt sich der Einkauf dir nicht zuordnen — versuch es gleich noch einmal.',
  merchantNotFound: 'Dieser Shop ist nicht verfügbar.',

  withdraw: 'Auszahlen',
  withdrawTitle: 'Auf dein Konto auszahlen',
  withdrawIntro:
    'Nur bestätigte Beträge. Offene Buchungen bleiben liegen, bis der Partner sie bestätigt.',
  amountLabel: 'Betrag',
  destinationLabel: 'Ziel',
  destinationUnverified: 'Noch nicht verifiziert',
  noVerifiedDestination:
    'Kein verifiziertes Ziel vorhanden. Ausgezahlt wird nur auf ein Konto, das dir gehört und verifiziert ist.',
  whatHappensNext: 'Was als Nächstes passiert',
  approvalStep1: 'Der Auftrag wird erfasst und der Betrag sofort reserviert.',
  approvalStep2: 'Eine namentlich genannte Person gibt ihn frei — jede Auszahlung, ohne Ausnahme.',
  approvalStep3: 'Nach der Freigabe geht die Zahlung raus und du bekommst einen Beleg.',
  wholeEntriesRule:
    'Reserviert wird in ganzen Buchungen, älteste zuerst. Jede Buchung stützt sich auf genau einen Beleg des Netzwerks, es gibt also keine halbe zu reservieren — der reservierte Betrag kann höher sein als der angeforderte.',
  belowThreshold: (threshold) => `Der Mindestbetrag für eine Auszahlung ist ${threshold}.`,
  shortfall: (amount) => `Es fehlen ${amount}.`,
  requestRecorded: (reference) => `Auftrag erfasst: ${reference}`,
  reservedForRequest: (amount) => `${amount} reserviert`,
  withdrawalStates: {
    awaiting_approval: 'Wartet auf Freigabe',
    approved: 'Freigegeben',
    rejected: 'Abgelehnt',
    submitted: 'Zur Zahlung eingereicht',
    settled: 'Ausgezahlt',
    failed: 'Fehlgeschlagen',
  },
  decisionReason: 'Begründung',
  payoutReference: 'Zahlungsreferenz',
  noWithdrawals: 'Noch keine Auszahlungen.',

  notParticipating: 'Du nimmst noch nicht am Cashback teil.',
  optIn: 'Teilnehmen',

  operations: 'Betrieb',
  queueUnattributed: 'Ohne Zuordnung',
  queueHeld: 'Zurückgehalten',
  queueWithdrawals: 'Auszahlungen zur Freigabe',
  queueReconciliation: 'Abstimmung',
  oldestFirst: 'älteste zuerst',
  reasonLabel: 'Begründung',
  reasonRequired: 'Die Begründung ist Pflicht; ohne sie wird die Aktion abgelehnt.',
  decisionIsPermanent:
    'Die Entscheidung wird unter deinem Namen festgehalten und danach nicht mehr korrigiert — dieselbe Regel wie in der Redaktion.',
  attribute: 'Zuordnen',
  dismiss: 'Verwerfen',
  notAttributable:
    'Das Netzwerk nannte eine Referenz, zu der es keinen Klick gibt. Zulässig ist nur das Verwerfen.',
  release: 'Freigeben',
  reject: 'Ablehnen',
  approve: 'Freigeben',
  heldSince: 'Zurückgehalten seit',
  holdRule: 'Regel',
  networkReported: 'Das Netzwerk meldete',
  emptyQueue: 'In dieser Liste steht nichts offen.',
  differenceKinds: {
    reported_not_paid: 'Gemeldet, nicht gezahlt',
    amount_mismatch: 'Betragsabweichung',
    paid_not_reported: 'Gezahlt, nicht gemeldet',
  },
  expected: 'Erwartet',
  actual: 'Tatsächlich',
  delta: 'Differenz',
  superseded: 'Das Netzwerk hat den Beleg neu gemeldet',
  resolutionExplained: 'Erklärt',
  resolutionAbsorbed: 'Getragen',
  openDifferenceIsTheChase:
    'Eine offene Differenz ist die Nachforderung selbst und hält die Bestätigung zu. Sie wird nicht geschlossen, weil wir auf Geld warten.',
};

const CATALOGUES: Readonly<Record<ReadingLanguage, CashbackStrings>> = { el, de };

/** The catalogue for a reading language. */
export function cashbackStrings(lang: ReadingLanguage): CashbackStrings {
  return CATALOGUES[lang];
}

/**
 * The endonym of a language, for the "shown in German" note a fallback name
 * carries.
 *
 * `Intl.DisplayNames` in the *reading* language would render "Γερμανικά" on
 * a Greek page, which is right, but it needs a language tag the API sends
 * and the API sends a primary subtag. Anything it does not recognise falls
 * back to the tag itself rather than to a guess.
 */
export function languageName(lang: ReadingLanguage, subtag: string): string {
  try {
    return new Intl.DisplayNames([lang], { type: 'language' }).of(subtag) ?? subtag;
  } catch {
    return subtag;
  }
}
