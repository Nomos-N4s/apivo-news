import type { ReadingLanguage } from '../reader/axes';

/**
 * Chrome strings for the editorial screens, in the alpha languages —
 * editorial staff read the same two languages the product publishes in
 * (FR-015 keeps the route layer to el and de either way).
 */
export interface EditorialStrings {
  readonly editorial: string;
  readonly signedInAs: string;
  readonly roleEditor: string;
  /** Shown while the identity is a placeholder rather than a real sign-in. */
  readonly previewSession: string;
  /** Nav rail. */
  readonly reviewQueue: string;
  readonly sources: string;
  readonly published: string;
  readonly audit: string;
  readonly pending: string;
  /** Spend ledger (FR-006). */
  readonly translationSpend: string;
  readonly ofMonthlyCap: (cap: string, month: string) => string;
  /** Queue list. */
  readonly awaitingApproval: string;
  readonly newestFirst: string;
  readonly noTranslationNeeded: string;
  readonly queuedUntranslated: string;
  readonly queuedUntranslatedBody: (count: number) => string;
  readonly skippedOverCeiling: (count: number) => string;
  /** Review pane. */
  readonly reviewBeforeApproval: string;
  readonly retrievedImmutable: (time: string) => string;
  readonly originalIn: (language: string) => string;
  readonly translationIn: (language: string) => string;
  /** Right-hand column when the origin needs no translation at all. */
  readonly publishedAsIs: string;
  readonly openSource: string;
  readonly extractOnlyNote: string;
  readonly licenceAtRetrieval: string;
  readonly acknowledgement: string;
  readonly approveAndPublish: string;
  readonly reject: string;
  readonly skip: string;
  readonly rejectNote: string;
  /** Outcome banners. */
  readonly approvedTitle: string;
  readonly notRecordedTitle: string;
  readonly emptyQueue: string;
  readonly selectAnItem: string;
  /** Provenance audit (mockup 1h) — the five-minute trace (US5, FR-010). */
  readonly readOnlyAccess: string;
  readonly traceLabel: string;
  readonly trace: string;
  readonly publishedItem: string;
  readonly placesLabel: string;
  readonly sourceAndLicence: string;
  readonly snapshotNote: string;
  readonly translationLineage: string;
  readonly noTranslationLineage: string;
  readonly namedApprover: string;
  readonly eventStream: string;
  readonly withdrawTitle: string;
  readonly withdrawBody: string;
  readonly withdrawReasonLabel: string;
  readonly withdraw: string;
  readonly requiresEditorRole: string;
  readonly withdrawnAlready: string;
  readonly traceNotFound: string;
  /** Source management (mockup 1i) — the licensing invariant made visible. */
  readonly sourcesSummary: (configured: number, active: number) => string;
  readonly colSource: string;
  readonly colFeed: string;
  readonly colLang: string;
  readonly colUsageRule: string;
  readonly colLastPoll: string;
  readonly colState: string;
  readonly stateActive: string;
  readonly statePaused: string;
  readonly neverPolled: string;
  readonly lastPollCycle: string;
  readonly cycleSummary: (retrieved: number, duplicates: number) => string;
  readonly failures: string;
  readonly noFailures: string;
  readonly addSource: string;
  readonly publisherName: string;
  readonly feedUrl: string;
  readonly languageField: string;
  readonly jurisdictionField: string;
  readonly licenceTerms: string;
  readonly usageRuleHeading: string;
  readonly extractAndLinkTitle: string;
  readonly extractAndLinkBody: string;
  readonly fullTextTitle: string;
  readonly fullTextBody: string;
  readonly usageRuleNotAnInput: string;
  readonly addSourceAndPoll: string;
  readonly sourcesEmpty: string;
  /** Success confirmation; a 201 carries an id, not prose. */
  readonly sourceAdded: (id: string) => string;
}

const EL: EditorialStrings = {
  editorial: 'Σύνταξη',
  signedInAs: 'Συνδεδεμένος ως',
  roleEditor: 'ρόλος συντάκτη',
  previewSession: 'Προεπισκόπηση — δεν έχει γίνει πραγματική σύνδεση',
  reviewQueue: 'Ουρά ελέγχου',
  sources: 'Πηγές',
  published: 'Δημοσιευμένα',
  audit: 'Έλεγχος',
  pending: 'σε εκκρεμότητα',
  translationSpend: 'Δαπάνη μετάφρασης',
  ofMonthlyCap: (cap, month) => `από ${cap} μηνιαίο όριο · ${month}`,
  awaitingApproval: 'Αναμονή έγκρισης',
  newestFirst: 'νεότερα πρώτα',
  noTranslationNeeded: 'Δεν χρειάζεται μετάφραση',
  queuedUntranslated: 'Σε ουρά, χωρίς μετάφραση',
  queuedUntranslatedBody: (count) =>
    `${count} στοιχεία περιμένουν τον πάροχο μετάφρασης. Παραμένουν εδώ έως ότου απαντήσει — τίποτα δεν δημοσιεύεται μισοεπεξεργασμένο.`,
  skippedOverCeiling: (count) =>
    `${count} στοιχείο παραλείφθηκε: ξεπέρασε το ανώτατο κόστος ανά άρθρο. Σημειώθηκε, δεν μεταφράστηκε.`,
  reviewBeforeApproval: 'Έλεγχος πριν από την έγκριση',
  retrievedImmutable: (time) => `ανακτήθηκε ${time} · αμετάβλητο`,
  originalIn: (language) => `Πρωτότυπο — ${language}`,
  translationIn: (language) => `Μετάφραση — ${language}`,
  publishedAsIs: 'Θα δημοσιευθεί ως έχει',
  openSource: 'Άνοιγμα πηγής ↗',
  extractOnlyNote:
    'Μόνο τίτλος και απόσπασμα — πλήρης μετάφραση δεν επιτρέπεται για αυτή την πηγή.',
  licenceAtRetrieval: 'Άδεια κατά την ανάκτηση',
  acknowledgement:
    'Διάβασα το απόσπασμα και τους όρους της άδειας. Η έγκριση με καταγράφει ως τον επώνυμο εγκρίνοντα· η εγγραφή δεν μπορεί να τροποποιηθεί μετά.',
  approveAndPublish: 'Έγκριση και δημοσίευση',
  reject: 'Απόρριψη',
  skip: 'Παράλειψη',
  rejectNote:
    'Η απόρριψη δεν δημιουργεί άρθρο. Το ανακτημένο στοιχείο παραμένει ως τεκμήριο και στις δύο περιπτώσεις.',
  approvedTitle: 'Εγκρίθηκε',
  notRecordedTitle: 'Δεν καταγράφηκε',
  emptyQueue: 'Η ουρά είναι άδεια. Τίποτα δεν περιμένει έγκριση.',
  selectAnItem: 'Επιλέξτε ένα στοιχείο από την ουρά για έλεγχο.',
  readOnlyAccess: 'Πρόσβαση μόνο για ανάγνωση · ιδρυτής, νομικός σύμβουλος, εκδότης που ρωτά',
  traceLabel: 'Αναγνωριστικό άρθρου, διεύθυνση πηγής ή τίτλος',
  trace: 'Ιχνηλάτηση',
  publishedItem: 'Δημοσιευμένο στοιχείο',
  placesLabel: 'τόποι',
  sourceAndLicence: 'Πηγή και άδεια κατά την ανάκτηση',
  snapshotNote:
    'Αυτοί είναι οι όροι όπως ίσχυαν τότε, όχι οι όροι που είναι καταγεγραμμένοι σήμερα. Η υπεράσπιση στηρίζεται στο στιγμιότυπο.',
  translationLineage: 'Καταγωγή μετάφρασης',
  noTranslationLineage: 'καμία — η γλώσσα-στόχος ταυτίζεται',
  namedApprover: 'Επώνυμος εγκρίνων',
  eventStream: 'Ροή συμβάντων — μόνο προσθήκες',
  withdrawTitle: 'Απόσυρση από τη δημοσίευση',
  withdrawBody:
    'Η απόσυρση τερματίζει τη δημοσίευση. Το άρθρο, η έγκρισή του και τα ανακτημένα τεκμήρια παραμένουν, και η ίδια η ενέργεια καταγράφεται. Τίποτα δεν διαγράφεται.',
  withdrawReasonLabel: 'Αιτιολογία (καταγράφεται στη ροή συμβάντων)',
  withdraw: 'Απόσυρση',
  requiresEditorRole: 'Απαιτεί ρόλο συντάκτη',
  withdrawnAlready: 'Έχει ήδη αποσυρθεί',
  traceNotFound: 'Δεν βρέθηκε άρθρο με αυτό το αναγνωριστικό.',
  sourcesSummary: (configured, active) =>
    `${configured} ρυθμισμένες · ${active} ενεργές · μόνο RSS και Atom, χωρίς σάρωση`,
  colSource: 'Πηγή',
  colFeed: 'Ροή',
  colLang: 'Γλώσσα',
  colUsageRule: 'Κανόνας χρήσης',
  colLastPoll: 'Τελευταία λήψη',
  colState: 'Κατάσταση',
  stateActive: 'Ενεργή',
  statePaused: 'Σε παύση',
  neverPolled: 'ποτέ',
  lastPollCycle: 'Τελευταίος κύκλος λήψης',
  cycleSummary: (retrieved, duplicates) =>
    `${retrieved} στοιχεία ανακτήθηκαν · ${duplicates} διπλότυπα παραλείφθηκαν από το αποτύπωμα`,
  failures: 'Αστοχίες',
  noFailures: 'Καμία στον τελευταίο κύκλο',
  addSource: 'Προσθήκη πηγής',
  publisherName: 'Όνομα εκδότη',
  feedUrl: 'Διεύθυνση ροής (RSS ή Atom)',
  languageField: 'Γλώσσα',
  jurisdictionField: 'Δικαιοδοσία',
  licenceTerms: 'Όροι άδειας που τηρούνται',
  usageRuleHeading: 'Κανόνας χρήσης',
  extractAndLinkTitle: 'extract_and_link',
  extractAndLinkBody:
    'Η προεπιλογή, και ο μόνος κανόνας που ισχύει χωρίς καταγεγραμμένη γραπτή άδεια.',
  fullTextTitle: 'full_text',
  fullTextBody:
    'Μη διαθέσιμο. Απαιτείται πρώτα καταγεγραμμένη γραπτή άδεια· διαφορετικά η βάση δεδομένων το απορρίπτει.',
  usageRuleNotAnInput:
    'Ο κανόνας δεν είναι πεδίο της φόρμας: κάθε νέα πηγή είναι extract_and_link και η αναβάθμιση είναι ξεχωριστή διαδικασία με έγκριση ιδρυτή.',
  addSourceAndPoll: 'Προσθήκη πηγής και έναρξη λήψης',
  sourcesEmpty: 'Δεν έχει ρυθμιστεί καμία πηγή ακόμη.',
  sourceAdded: (id) => `Η πηγή ρυθμίστηκε (${id}) και η λήψη ξεκίνησε.`,
};

const DE: EditorialStrings = {
  editorial: 'Redaktion',
  signedInAs: 'Angemeldet als',
  roleEditor: 'Rolle Redaktion',
  previewSession: 'Vorschau — keine echte Anmeldung',
  reviewQueue: 'Prüfliste',
  sources: 'Quellen',
  published: 'Veröffentlicht',
  audit: 'Prüfpfad',
  pending: 'ausstehend',
  translationSpend: 'Übersetzungskosten',
  ofMonthlyCap: (cap, month) => `von ${cap} Monatslimit · ${month}`,
  awaitingApproval: 'Warten auf Freigabe',
  newestFirst: 'neueste zuerst',
  noTranslationNeeded: 'Keine Übersetzung nötig',
  queuedUntranslated: 'In Warteschlange, unübersetzt',
  queuedUntranslatedBody: (count) =>
    `${count} Beiträge warten auf den Übersetzungsanbieter. Sie bleiben hier, bis er antwortet — nichts wird halbfertig veröffentlicht.`,
  skippedOverCeiling: (count) =>
    `${count} Beitrag übersprungen: Kostenobergrenze pro Artikel überschritten. Vermerkt, nicht übersetzt.`,
  reviewBeforeApproval: 'Prüfung vor der Freigabe',
  retrievedImmutable: (time) => `abgerufen ${time} · unveränderlich`,
  originalIn: (language) => `Original — ${language}`,
  translationIn: (language) => `Übersetzung — ${language}`,
  publishedAsIs: 'Wird unverändert veröffentlicht',
  openSource: 'Quelle öffnen ↗',
  extractOnlyNote:
    'Nur Überschrift und Auszug — eine Volltextübersetzung ist für diese Quelle nicht zulässig.',
  licenceAtRetrieval: 'Lizenz beim Abruf',
  acknowledgement:
    'Ich habe den Auszug und die Lizenzbedingungen gelesen. Die Freigabe verzeichnet mich namentlich als freigebende Person; der Eintrag kann danach nicht geändert werden.',
  approveAndPublish: 'Freigeben und veröffentlichen',
  reject: 'Ablehnen',
  skip: 'Überspringen',
  rejectNote:
    'Eine Ablehnung erzeugt keinen Artikel. Der abgerufene Beitrag bleibt in beiden Fällen als Nachweis erhalten.',
  approvedTitle: 'Freigegeben',
  notRecordedTitle: 'Nicht verzeichnet',
  emptyQueue: 'Die Liste ist leer. Nichts wartet auf Freigabe.',
  selectAnItem: 'Wählen Sie einen Beitrag aus der Liste zur Prüfung.',
  readOnlyAccess: 'Nur-Lese-Zugriff · Gründung, Rechtsberatung, anfragender Verlag',
  traceLabel: 'Artikel-ID, Quell-URL oder Überschrift',
  trace: 'Nachverfolgen',
  publishedItem: 'Veröffentlichter Beitrag',
  placesLabel: 'Orte',
  sourceAndLicence: 'Quelle und Lizenz beim Abruf',
  snapshotNote:
    'Das sind die Bedingungen, wie sie damals galten, nicht die heute hinterlegten. Die Verteidigung stützt sich auf den Schnappschuss.',
  translationLineage: 'Übersetzungsherkunft',
  noTranslationLineage: 'keine — Zielsprache stimmt überein',
  namedApprover: 'Namentliche Freigabe',
  eventStream: 'Ereignisstrom — nur Anfügen',
  withdrawTitle: 'Aus der Veröffentlichung zurückziehen',
  withdrawBody:
    'Der Rückzug beendet die Veröffentlichung. Der Artikel, seine Freigabe und die abgerufenen Nachweise bleiben erhalten, und der Vorgang selbst wird verzeichnet. Nichts wird gelöscht.',
  withdrawReasonLabel: 'Begründung (wird im Ereignisstrom verzeichnet)',
  withdraw: 'Zurückziehen',
  requiresEditorRole: 'Erfordert die Redaktionsrolle',
  withdrawnAlready: 'Bereits zurückgezogen',
  traceNotFound: 'Zu dieser Kennung wurde kein Artikel gefunden.',
  sourcesSummary: (configured, active) =>
    `${configured} eingerichtet · ${active} aktiv · nur RSS und Atom, kein Scraping`,
  colSource: 'Quelle',
  colFeed: 'Feed',
  colLang: 'Sprache',
  colUsageRule: 'Nutzungsregel',
  colLastPoll: 'Letzter Abruf',
  colState: 'Status',
  stateActive: 'Aktiv',
  statePaused: 'Pausiert',
  neverPolled: 'nie',
  lastPollCycle: 'Letzter Abrufzyklus',
  cycleSummary: (retrieved, duplicates) =>
    `${retrieved} Beiträge abgerufen · ${duplicates} Dubletten per Fingerabdruck übersprungen`,
  failures: 'Fehlschläge',
  noFailures: 'Keine im letzten Zyklus',
  addSource: 'Quelle hinzufügen',
  publisherName: 'Name des Verlags',
  feedUrl: 'Feed-URL (RSS oder Atom)',
  languageField: 'Sprache',
  jurisdictionField: 'Rechtsraum',
  licenceTerms: 'Hinterlegte Lizenzbedingungen',
  usageRuleHeading: 'Nutzungsregel',
  extractAndLinkTitle: 'extract_and_link',
  extractAndLinkBody:
    'Die Vorgabe und die einzige Regel, die ohne hinterlegte schriftliche Erlaubnis gilt.',
  fullTextTitle: 'full_text',
  fullTextBody:
    'Nicht verfügbar. Zuerst muss eine schriftliche Erlaubnis hinterlegt sein; sonst weist die Datenbank es zurück.',
  usageRuleNotAnInput:
    'Die Regel ist kein Formularfeld: jede neue Quelle ist extract_and_link, und eine Heraufstufung ist ein eigener, von der Gründung freigegebener Vorgang.',
  addSourceAndPoll: 'Quelle hinzufügen und Abruf starten',
  sourcesEmpty: 'Noch keine Quelle eingerichtet.',
  sourceAdded: (id) => `Quelle eingerichtet (${id}); der Abruf läuft.`,
};

const STRINGS: Readonly<Record<ReadingLanguage, EditorialStrings>> = { el: EL, de: DE };

/** The editorial chrome strings for a language. */
export function editorialStrings(lang: ReadingLanguage): EditorialStrings {
  return STRINGS[lang];
}

/** Language names for the original/translation column headings. */
const LANGUAGE_NAMES: Readonly<Record<string, Readonly<Record<string, string>>>> = {
  el: { el: 'Ελληνικά', de: 'Γερμανικά', en: 'Αγγλικά' },
  de: { el: 'Griechisch', de: 'Deutsch', en: 'Englisch' },
};

/** Names a content language in the reading language; unknown codes stay as-is. */
export function languageName(lang: ReadingLanguage, code: string): string {
  return LANGUAGE_NAMES[lang]?.[code] ?? code;
}
