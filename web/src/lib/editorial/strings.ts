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
  /** The other value of `account.role`; the chrome names what it found. */
  readonly roleReader: string;
  /**
   * Shown when what the screen displays is fixture data rather than the
   * API's records — keyed to the data's own provenance, because invented
   * numbers must never present as real, least of all to a signed-in
   * editor whose decisions they would inform.
   */
  readonly previewData: string;
  /** The chrome when nobody is signed in — it names no one. */
  readonly notSignedIn: string;
  readonly signIn: string;
  readonly signOut: string;
  /** The sign-in screen. */
  readonly signInTitle: string;
  readonly signInIntro: string;
  readonly signInFailed: string;
  readonly signInUnavailable: string;
  readonly signedOutNow: string;
  /**
   * A POST arrived from a session that had already ended. The page keeps
   * what was typed on screen instead of bouncing it through a redirect.
   */
  readonly signedOutMidPost: string;
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
  /**
   * The visible stand-in when the feed declared no publication date. The
   * attribution is frozen at approval, so an absent date must be SAID
   * rather than papered over with the retrieval date — a different claim,
   * and one that would be frozen in permanently (#87).
   */
  readonly publicationDateNotSupplied: string;
  /**
   * The place checkbox group's heading. The front page is scoped by place
   * (FR-009), so where an article publishes to is part of the approval —
   * an article tagged to no place could never appear anywhere.
   */
  readonly publishTo: string;
  /** The not-recorded reason when the form arrives with no place checked. */
  readonly atLeastOnePlace: string;
  readonly acknowledgement: string;
  readonly approveAndPublish: string;
  readonly reject: string;
  readonly skip: string;
  readonly rejectNote: string;
  /**
   * The queue row lacks the evidence block (#87) — an API predating it —
   * so the approve button is disabled: a permanent approval is not given
   * over placeholder dashes.
   */
  readonly evidenceIncomplete: string;
  /** The tag on a re-queued origin whose earlier publication was withdrawn. */
  readonly correctionTag: string;
  /** The review-pane note explaining what the correction tag means. */
  readonly correctionBody: string;
  /**
   * Signed in, yet the editorial API refuses every call: the quickstart's
   * documented first-run state, where no `account` row has been
   * provisioned for this person. Named plainly — the generic outage body
   * would send the operator hunting for a failure that is not there.
   */
  readonly notProvisionedTitle: string;
  readonly notProvisionedBody: string;
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
  /**
   * The page bound was reached with more pages on offer: the table and
   * the summary count cover what was fetched, not the whole registry,
   * and the screen must say so rather than truncate silently.
   */
  readonly sourcesTruncated: string;
  /** Success confirmation; a 201 carries an id, not prose. */
  readonly sourceAdded: (id: string) => string;
  /** Source management (#118): selection, bulk bar, edit flow, view toggle. */
  readonly selectAll: string;
  readonly selectRow: (name: string) => string;
  readonly bulkActivate: string;
  readonly bulkDeactivate: string;
  readonly bulkDelete: string;
  /**
   * What the two removals mean: deactivation is the everyday one and
   * keeps history; deletion is accepted only where no evidence exists,
   * because the database refuses to destroy a provenance chain.
   */
  readonly bulkBarNote: string;
  /** A bulk POST arrived with nothing selected: nothing was done, say so. */
  readonly noneSelected: string;
  /**
   * The bulk summaries state exactly what happened - both counts, always,
   * and the delete summary says refusals were NOT converted into
   * deactivations. No partial-success lie.
   */
  readonly bulkActivateSummary: (recorded: number, refused: number) => string;
  readonly bulkDeactivateSummary: (recorded: number, refused: number) => string;
  readonly bulkDeleteSummary: (recorded: number, refused: number) => string;
  readonly editSource: string;
  readonly editingSource: (name: string) => string;
  /** Edits are licensing events: the audit stream records old and new. */
  readonly editAudited: string;
  readonly activeField: string;
  readonly saveChanges: string;
  readonly cancel: string;
  readonly sourceUpdated: string;
  /**
   * Every field came back exactly as the form showed it, so nothing was
   * sent. Not a failure and not a save - a finding, said plainly, because
   * confirming it as an edit would claim a record that was never written.
   */
  readonly sourceUnchanged: string;
  /**
   * The edit arrived without the values it was rendered with, so what the
   * editor changed cannot be told from what they did not. Sending all
   * four would revert whatever another editor altered meanwhile, so
   * nothing is sent and the form is offered again.
   */
  readonly editFormIncomplete: string;
  /** The all/active/inactive toggle over the table. */
  readonly viewLabel: string;
  readonly viewAll: string;
  readonly viewActive: string;
  readonly viewInactive: string;
}

const EL: EditorialStrings = {
  editorial: 'Σύνταξη',
  signedInAs: 'Συνδεδεμένος ως',
  roleEditor: 'ρόλος συντάκτη',
  roleReader: 'ρόλος αναγνώστη',
  previewData: 'Δείγματα δεδομένων — καμία πραγματική εγγραφή',
  notSignedIn: 'Καμία σύνδεση',
  signIn: 'Σύνδεση',
  signOut: 'Αποσύνδεση',
  signInTitle: 'Σύνδεση συντάκτη',
  signInIntro:
    'Η έγκριση καταγράφει το όνομά σας μόνιμα δίπλα στο άρθρο. Συνδεθείτε με τον δικό σας λογαριασμό — ποτέ κοινόχρηστο.',
  signInFailed: 'Η σύνδεση δεν έγινε δεκτή. Ελέγξτε το ηλεκτρονικό ταχυδρομείο και τον κωδικό.',
  signInUnavailable:
    'Δεν έχει ρυθμιστεί υπηρεσία ταυτοποίησης σε αυτή την εγκατάσταση, οπότε η φόρμα δεν στέλνει τίποτα. Οι συντακτικές οθόνες δείχνουν δείγματα δεδομένων.',
  signedOutNow: 'Αποσυνδεθήκατε.',
  signedOutMidPost:
    'Η συνεδρία σας έληξε πριν από την υποβολή, οπότε τίποτα δεν καταγράφηκε. Συνδεθείτε ξανά — ό,τι πληκτρολογήσατε διατηρείται εδώ.',
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
  publicationDateNotSupplied: 'η ροή δεν δήλωσε ημερομηνία δημοσίευσης',
  publishTo: 'Δημοσίευση σε',
  atLeastOnePlace:
    'Επιλέξτε τουλάχιστον έναν τόπο: η πρώτη σελίδα φιλτράρεται ανά τόπο, οπότε ένα άρθρο χωρίς τόπο δεν θα εμφανιζόταν πουθενά. Τίποτα δεν καταγράφηκε.',
  acknowledgement:
    'Διάβασα το απόσπασμα και τους όρους της άδειας. Η έγκριση με καταγράφει ως τον επώνυμο εγκρίνοντα· η εγγραφή δεν μπορεί να τροποποιηθεί μετά.',
  approveAndPublish: 'Έγκριση και δημοσίευση',
  reject: 'Απόρριψη',
  skip: 'Παράλειψη',
  rejectNote:
    'Η απόρριψη δεν δημιουργεί άρθρο. Το ανακτημένο στοιχείο παραμένει ως τεκμήριο και στις δύο περιπτώσεις.',
  evidenceIncomplete:
    'Η έγκριση απενεργοποιήθηκε: η εγγραφή δεν φέρει τα πλήρη αποδεικτικά στοιχεία (πρωτότυπο κείμενο, σύνδεσμο, αποτύπωμα, προέλευση μετάφρασης). Μια μόνιμη έγκριση δεν δίνεται πάνω σε κενά.',
  correctionTag: 'Διόρθωση',
  correctionBody:
    'Η προηγούμενη δημοσίευση αυτής της προέλευσης αποσύρθηκε· ελέγχεται ως διόρθωση, όχι ως πρώτη έγκριση.',
  notProvisionedTitle: 'Ο λογαριασμός σας δεν έχει καταχωριστεί ως συντάκτης',
  notProvisionedBody:
    'Η σύνδεση έγινε, αλλά το συντακτικό API απορρίπτει κάθε κλήση: δεν υπάρχει εγγραφή account για τον λογαριασμό σας στη βάση δεδομένων του API. Χρειάζεται το βήμα «Provision an editor» του quickstart (specs/001-epiloyes-alpha/quickstart.md) — μία εγγραφή account με το Supabase user id σας και ρόλο editor, στη βάση όπου δείχνει το DATABASE_URL.',
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
  sourcesTruncated:
    'Υπάρχουν κι άλλες πηγές που δεν εμφανίζονται εδώ· ο πίνακας και τα σύνολα καλύπτουν όσες φορτώθηκαν.',
  sourceAdded: (id) => `Η πηγή ρυθμίστηκε (${id}) και η λήψη ξεκίνησε.`,
  selectAll: 'Επιλογή όλων',
  selectRow: (name) => `Επιλογή: ${name}`,
  bulkActivate: 'Ενεργοποίηση',
  bulkDeactivate: 'Απενεργοποίηση',
  bulkDelete: 'Διαγραφή',
  bulkBarNote:
    'Η απενεργοποίηση είναι η καθημερινή «αφαίρεση»: σταματά τη λήψη και το ιστορικό μένει άθικτο. Η διαγραφή γίνεται δεκτή μόνο για πηγές χωρίς κανένα ανακτημένο τεκμήριο — διαφορετικά η βάση δεδομένων αρνείται.',
  noneSelected: 'Δεν επιλέχθηκε καμία πηγή, οπότε τίποτα δεν έγινε.',
  bulkActivateSummary: (recorded, refused) =>
    `${recorded} ενεργοποιήθηκαν · ${refused} απορρίφθηκαν — για αυτές δεν καταγράφηκε καμία αλλαγή`,
  bulkDeactivateSummary: (recorded, refused) =>
    `${recorded} απενεργοποιήθηκαν · ${refused} απορρίφθηκαν — για αυτές δεν καταγράφηκε καμία αλλαγή`,
  bulkDeleteSummary: (recorded, refused) =>
    `${recorded} διαγράφηκαν · ${refused} απορρίφθηκαν — παραμένουν ως έχουν· καμία δεν μετατράπηκε σιωπηλά σε απενεργοποίηση`,
  editSource: 'Επεξεργασία',
  editingSource: (name) => `Επεξεργασία: ${name}`,
  editAudited:
    'Κάθε αλλαγή καταγράφεται στη ροή συμβάντων με την παλιά και τη νέα τιμή — οι όροι άδειας είναι μέρος του αρχείου. Τα στιγμιότυπα των ήδη ανακτημένων τεκμηρίων δεν αλλάζουν.',
  activeField: 'Ενεργή — γίνεται λήψη',
  saveChanges: 'Αποθήκευση αλλαγών',
  cancel: 'Άκυρο',
  sourceUpdated: 'Οι αλλαγές καταγράφηκαν.',
  sourceUnchanged:
    'Κανένα πεδίο δεν άλλαξε σε σχέση με αυτό που εμφάνιζε η φόρμα, οπότε δεν στάλθηκε τίποτα και δεν καταγράφηκε καμία αλλαγή.',
  editFormIncomplete:
    'Η φόρμα δεν έφερε τις τιμές με τις οποίες εμφανίστηκε, οπότε δεν ήταν δυνατό να ξεχωριστεί τι άλλαξε. Δεν στάλθηκε τίποτα — δοκιμάστε ξανά από τη λίστα.',
  viewLabel: 'Προβολή',
  viewAll: 'Όλες',
  viewActive: 'Ενεργές',
  viewInactive: 'Ανενεργές',
};

const DE: EditorialStrings = {
  editorial: 'Redaktion',
  signedInAs: 'Angemeldet als',
  roleEditor: 'Rolle Redaktion',
  roleReader: 'Rolle Lesen',
  previewData: 'Beispieldaten — keine echten Einträge',
  notSignedIn: 'Nicht angemeldet',
  signIn: 'Anmelden',
  signOut: 'Abmelden',
  signInTitle: 'Anmeldung Redaktion',
  signInIntro:
    'Eine Freigabe verzeichnet Ihren Namen dauerhaft neben dem Artikel. Melden Sie sich mit Ihrem eigenen Konto an — niemals mit einem geteilten.',
  signInFailed: 'Die Anmeldung wurde nicht angenommen. Prüfen Sie E-Mail und Passwort.',
  signInUnavailable:
    'In dieser Installation ist kein Anmeldedienst eingerichtet, dieses Formular sendet also nichts. Die Redaktionsansichten zeigen Beispieldaten.',
  signedOutNow: 'Sie sind abgemeldet.',
  signedOutMidPost:
    'Ihre Sitzung endete vor dem Absenden, es wurde also nichts verzeichnet. Melden Sie sich erneut an — das Eingetippte bleibt hier erhalten.',
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
  publicationDateNotSupplied: 'der Feed hat kein Veröffentlichungsdatum angegeben',
  publishTo: 'Veröffentlichen in',
  atLeastOnePlace:
    'Wählen Sie mindestens einen Ort: die Titelseite ist nach Ort gefiltert, ein Artikel ohne Ort erschiene also nirgends. Es wurde nichts verzeichnet.',
  acknowledgement:
    'Ich habe den Auszug und die Lizenzbedingungen gelesen. Die Freigabe verzeichnet mich namentlich als freigebende Person; der Eintrag kann danach nicht geändert werden.',
  approveAndPublish: 'Freigeben und veröffentlichen',
  reject: 'Ablehnen',
  skip: 'Überspringen',
  rejectNote:
    'Eine Ablehnung erzeugt keinen Artikel. Der abgerufene Beitrag bleibt in beiden Fällen als Nachweis erhalten.',
  evidenceIncomplete:
    'Freigabe deaktiviert: dieser Eintrag trägt nicht die vollständige Beleglage (Originaltext, Link, Fingerabdruck, Übersetzungsherkunft). Eine dauerhafte Freigabe wird nicht über Lücken erteilt.',
  correctionTag: 'Korrektur',
  correctionBody:
    'Die frühere Veröffentlichung dieses Ursprungs wurde zurückgezogen; die Prüfung gilt einer Korrektur, nicht einer Erstfreigabe.',
  notProvisionedTitle: 'Ihr Konto ist nicht als Redaktion eingerichtet',
  notProvisionedBody:
    'Die Anmeldung war erfolgreich, aber das redaktionelle API weist jeden Aufruf zurück: für Ihr Konto gibt es keine account-Zeile in der Datenbank des API. Es fehlt der Schritt „Provision an editor“ aus dem Quickstart (specs/001-epiloyes-alpha/quickstart.md) — eine account-Zeile mit Ihrer Supabase-Benutzer-ID und der Rolle editor, in der Datenbank, auf die DATABASE_URL zeigt.',
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
  sourcesTruncated:
    'Es gibt weitere Quellen, die hier nicht angezeigt werden; Tabelle und Summen decken nur die geladenen ab.',
  sourceAdded: (id) => `Quelle eingerichtet (${id}); der Abruf läuft.`,
  selectAll: 'Alle auswählen',
  selectRow: (name) => `Auswählen: ${name}`,
  bulkActivate: 'Aktivieren',
  bulkDeactivate: 'Deaktivieren',
  bulkDelete: 'Löschen',
  bulkBarNote:
    'Deaktivieren ist das alltägliche „Entfernen“: der Abruf stoppt, die Historie bleibt unberührt. Löschen wird nur für Quellen ohne einen einzigen abgerufenen Nachweis angenommen — sonst weist die Datenbank es zurück.',
  noneSelected: 'Keine Quelle ausgewählt; es ist nichts geschehen.',
  bulkActivateSummary: (recorded, refused) =>
    `${recorded} aktiviert · ${refused} zurückgewiesen — für diese wurde nichts verzeichnet`,
  bulkDeactivateSummary: (recorded, refused) =>
    `${recorded} deaktiviert · ${refused} zurückgewiesen — für diese wurde nichts verzeichnet`,
  bulkDeleteSummary: (recorded, refused) =>
    `${recorded} gelöscht · ${refused} zurückgewiesen — sie bleiben bestehen; keine wurde stillschweigend deaktiviert`,
  editSource: 'Bearbeiten',
  editingSource: (name) => `Bearbeiten: ${name}`,
  editAudited:
    'Jede Änderung wird im Ereignisstrom mit altem und neuem Wert verzeichnet — die Lizenzbedingungen sind Teil des Archivs. Die Schnappschüsse bereits abgerufener Nachweise ändern sich nicht.',
  activeField: 'Aktiv — wird abgerufen',
  saveChanges: 'Änderungen speichern',
  cancel: 'Abbrechen',
  sourceUpdated: 'Die Änderungen wurden verzeichnet.',
  sourceUnchanged:
    'Kein Feld weicht von dem ab, was das Formular anzeigte; es wurde nichts gesendet und nichts verzeichnet.',
  editFormIncomplete:
    'Das Formular trug die Werte nicht mit, mit denen es angezeigt wurde, also ließ sich nicht unterscheiden, was geändert wurde. Es wurde nichts gesendet — bitte erneut aus der Liste heraus bearbeiten.',
  viewLabel: 'Ansicht',
  viewAll: 'Alle',
  viewActive: 'Aktive',
  viewInactive: 'Inaktive',
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
