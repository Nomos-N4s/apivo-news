import type { PlaceScope, ReadingLanguage } from './axes';

/**
 * Reader-facing chrome strings for the alpha languages (FR-015). Content
 * arrives from the API already in the reading language; these cover only
 * what the frontend itself says. Keyed by the language axis — never by any
 * combined locale (FR-009).
 */
export interface UiStrings {
  /** `<title>`/description tagline — also the brand's one-line self-description. */
  readonly tagline: string;
  /** Label for the language axis control. */
  readonly readingLanguage: string;
  /** Label for the place axis control. */
  readonly places: string;
  readonly editPlaces: string;
  /** Kicker scope labels by structural place rank. */
  readonly scopeLabels: Readonly<Record<PlaceScope, string>>;
  /** Lead attribution prefix: "Originally published by …". */
  readonly originallyPublishedBy: string;
  /** Summary label of the provenance disclosure. */
  readonly provenance: string;
  /** Disclosure row terms — the public record fields. */
  readonly source: string;
  readonly published: string;
  readonly attribution: string;
  /** Record-row term for the approval time (public payload's approved_at). */
  readonly approved: string;
  /** The article page's Record rail heading (mockup 1c). */
  readonly record: string;
  /** The extract-boundary block (mockup 1c): the page is a doorway, and says so. */
  readonly wholeExtractTitle: string;
  readonly wholeExtractBody: string;
  /** The primary CTA to the publisher: "Continue at {host} ↗" (SC-008). */
  readonly continueAt: (host: string) => string;
  /** 1d's plain-language note, adopted under the CTA. */
  readonly extractNote: string;
  /** The related rail heading: "More from {place}". */
  readonly moreFrom: (placeName: string) => string;
  /** Empty-state band for a followed place with nothing published (US1-AC3). */
  readonly emptyPlaceTitle: string;
  readonly emptyPlaceBody: (placeName: string) => string;
  /** The reassurance footer line (mockup 1a/1b). */
  readonly reassurance: string;
  /** Calm not-found copy. */
  readonly notFoundTitle: string;
  readonly notFoundBody: string;
  /** Calm degraded state when the reader API cannot be reached (503). */
  readonly unavailableTitle: string;
  readonly unavailableBody: string;
  /**
   * The fixture marker (issue #134). Shown whenever the page was rendered
   * from the built-in sample data rather than the published record — the
   * one state in which the names on the page are not real people and the
   * articles were approved by nobody. It is not decoration and not a
   * developer aid: an unmarked fixture page is indistinguishable from the
   * newspaper, which is the failure this exists to prevent.
   */
  readonly fixtureNoticeTitle: string;
  readonly fixtureNoticeBody: string;
  /** Masthead sign-in (mockup 1a) — disabled until registration ships (T031). */
  readonly signIn: string;
  readonly signInPending: string;
  /**
   * Site footer. The imprint and privacy notice are not decoration: a
   * German-facing service owes an Impressum (TMG §5) and a GDPR privacy
   * notice, so the footer carries their places and says openly that they
   * are still owed rather than linking to pages that do not exist.
   */
  readonly alphaLabel: string;
  readonly imprint: string;
  readonly privacy: string;
  readonly contact: string;
  readonly legalPending: string;
  /**
   * The about page (issue #92, FR-016). Every statement below derives from
   * the spec or from shipped behaviour — the page describes the pipeline
   * the product actually runs, never aspirations.
   */
  readonly aboutLabel: string;
  /**
   * The way in to the editorial screens, from any reader page.
   *
   * Not the reader's `signIn` above, which is a disabled placeholder for a
   * capability that is still on the cut list. This one leads somewhere that
   * exists, and without it the only route to /{lang}/editor/signin was
   * typing it — which is how the editorial staff found out.
   */
  readonly editorialLabel: string;
  readonly aboutHeading: string;
  readonly aboutIntro: string;
  readonly aboutPipelineHeading: string;
  readonly aboutPipelineParagraphs: readonly string[];
  readonly aboutTranslationHeading: string;
  readonly aboutTranslationParagraphs: readonly string[];
  readonly aboutCorrectionsHeading: string;
  readonly aboutCorrectionsParagraphs: readonly string[];
  readonly aboutPublishersHeading: string;
  readonly aboutPublishersParagraphs: readonly string[];
  /** Registration and consent (mockup 1j, US6/FR-011). */
  readonly createAccount: string;
  readonly accountSubtitle: string;
  readonly nameLabel: string;
  readonly emailLabel: string;
  readonly passwordLabel: string;
  readonly placesToFollow: string;
  readonly followNote: string;
  readonly registrationPending: string;
  readonly consentHeading: string;
  readonly consentIntro: string;
  readonly purposeLabels: Readonly<Record<string, string>>;
  readonly purposeWord: string;
  readonly granted: string;
  readonly notGranted: string;
  readonly grant: string;
  readonly revoke: string;
  readonly consentHistoryHeading: string;
  readonly current: string;
  readonly recordCount: (count: number) => string;
  readonly notRecorded: string;
  /** Success: a consent record was actually written. */
  readonly consentRecorded: string;
  readonly consentGrantedNow: string;
  readonly consentRevokedNow: string;
  /** The axis bar (mockup 1f) — chip actions and the independence line. */
  readonly addPlace: (placeName: string) => string;
  readonly removePlace: (placeName: string) => string;
  readonly independenceLine: string;
  /** The setup dialog (mockup 1e). */
  readonly setupTitle: string;
  readonly setupSubtitle: string;
  readonly axisOneLabel: string;
  readonly axisTwoLabel: string;
  readonly alphaLanguagesNote: string;
  readonly selectedCount: (count: number) => string;
  readonly covers: (placeName: string) => string;
  readonly frontPagePreviewLabel: string;
  readonly later: string;
  readonly startReading: string;
}

const EL: UiStrings = {
  tagline: 'Τοπικές ειδήσεις για τις ελληνικές κοινότητες του εξωτερικού.',
  readingLanguage: 'Γλώσσα ανάγνωσης',
  places: 'Τόποι',
  editPlaces: 'Αλλαγή τόπων',
  scopeLabels: { city: 'Τοπικά', region: 'Περιφέρεια', country: 'Εθνικά' },
  originallyPublishedBy: 'Δημοσιεύθηκε αρχικά από',
  provenance: 'Προέλευση',
  source: 'Πηγή',
  published: 'Δημοσίευση',
  attribution: 'Απόδοση',
  approved: 'Έγκριση',
  record: 'Μητρώο',
  wholeExtractTitle: 'Αυτό είναι ολόκληρο το απόσπασμα',
  wholeExtractBody:
    'Η άδεια με αυτόν τον εκδότη καλύπτει αποδιδόμενο τίτλο και σύντομο απόσπασμα. Το υπόλοιπο άρθρο παραμένει εκεί όπου γράφτηκε.',
  continueAt: (host) => `Συνέχεια στο ${host} ↗`,
  extractNote: 'Μόνο απόσπασμα, βάσει άδειας. Ένα κλικ έως τον εκδότη.',
  moreFrom: (placeName) => `Περισσότερα από ${placeName}`,
  emptyPlaceTitle: 'Τόπος χωρίς δημοσιεύσεις ακόμη',
  emptyPlaceBody: (placeName) =>
    `Δεν έχει δημοσιευθεί ακόμη τίποτα για ${placeName}. Τα εγκεκριμένα άρθρα θα εμφανίζονται εδώ.`,
  reassurance:
    'Κάθε άρθρο προέρχεται από αδειοδοτημένη ροή, αποδίδεται μόνο ως τίτλος και απόσπασμα — ποτέ πλήρες κείμενο — και εγκρίνεται από επώνυμο συντάκτη πριν εμφανιστεί.',
  notFoundTitle: 'Δεν υπάρχει κάτι εδώ.',
  notFoundBody: 'Η διεύθυνση δεν αντιστοιχεί σε δημοσιευμένη σελίδα.',
  unavailableTitle: 'Η σελίδα δεν είναι διαθέσιμη αυτή τη στιγμή',
  unavailableBody:
    'Δοκιμάστε ξανά σε λίγο. Δεν χάθηκε τίποτα — τα άρθρα θα εμφανιστούν μόλις αποκατασταθεί η σύνδεση.',
  fixtureNoticeTitle: 'Δείγμα δεδομένων — όχι δημοσιευμένη ύλη',
  fixtureNoticeBody:
    'Αυτή η σελίδα δεν συνδέεται με τη βάση δεδομένων. Τα άρθρα, οι εκδότες και τα ονόματα των συντακτών παρακάτω είναι επινοημένα: κανένα δεν εγκρίθηκε από άνθρωπο και κανένα δεν αποτελεί δημοσιευμένη ύλη.',
  signIn: 'Σύνδεση',
  signInPending: 'Διαθέσιμο με την εγγραφή',
  alphaLabel: 'Άλφα',
  imprint: 'Ταυτότητα',
  privacy: 'Απόρρητο',
  contact: 'Επικοινωνία',
  legalPending: 'εκκρεμούν πριν από τη δημόσια κυκλοφορία',
  aboutLabel: 'Σχετικά',
  editorialLabel: 'Σύνταξη',
  aboutHeading: 'Σχετικά με το epiloYES',
  aboutIntro:
    'Το epiloYES είναι μια πολύγλωσση τοπική εφημερίδα για τις ελληνικές κοινότητες του εξωτερικού. Η γλώσσα ανάγνωσης και οι τόποι που παρακολουθείτε είναι δύο ανεξάρτητοι άξονες: διαβάζετε στα ελληνικά ή στα γερμανικά, για όποιους τόπους σας αφορούν — η αλλαγή του ενός δεν αγγίζει ποτέ τον άλλον.',
  aboutPipelineHeading: 'Πώς δουλεύει',
  aboutPipelineParagraphs: [
    'Παρακολουθούμε αδειοδοτημένες πηγές ειδήσεων και παίρνουμε από αυτές μόνο ό,τι επιτρέπει η άδειά τους: τίτλο και σύντομο απόσπασμα, πάντα με σύνδεσμο προς το πρωτότυπο.',
    'Τίποτα δεν δημοσιεύεται χωρίς επώνυμη ανθρώπινη έγκριση. Κάθε άρθρο εγκρίνεται από επώνυμο μέλος της σύνταξης πριν εμφανιστεί σε οποιαδήποτε πρώτη σελίδα.',
    'Κάθε άρθρο συνοδεύεται από το αρχείο του: στη σελίδα του βλέπετε την πηγή και τον χρόνο έγκρισης, ενώ το πλήρες πρωτόκολλο — άδεια χρήσης, μοντέλο μετάφρασης, έκδοση εντολής, εγκρίνων — καταγράφεται μόνιμα.',
  ],
  aboutTranslationHeading: 'Μηχανική μετάφραση, ανθρώπινη ευθύνη',
  aboutTranslationParagraphs: [
    'Οι μεταφράσεις γίνονται από μηχανή και ελέγχονται από επώνυμο μέλος της σύνταξης πριν από τη δημοσίευση. Το πρωτότυπο συνδέεται πάντα δίπλα στο απόσπασμα, ώστε να μπορείτε να ανατρέξετε στην πηγή.',
  ],
  aboutCorrectionsHeading: 'Διορθώσεις και αποσύρσεις',
  aboutCorrectionsParagraphs: [
    'Όταν κάνουμε λάθος, το διορθώνουμε. Όταν ένα άρθρο αποσυρθεί, η σελίδα του το δηλώνει καθαρά — δεν προσποιούμαστε ότι δεν υπήρξε ποτέ. Το αρχείο δημοσίευσης δεν σβήνεται ούτε ξαναγράφεται.',
  ],
  aboutPublishersHeading: 'Για εκδότες',
  aboutPublishersParagraphs: [
    'Χρησιμοποιούμε μόνο ό,τι επιτρέπει ρητά η εκάστοτε άδεια: απόσπασμα και σύνδεσμο, ποτέ πλήρες κείμενο χωρίς γραπτή άδεια. Αν είστε εκδότης και έχετε ερωτήσεις για την άδειά σας, τα στοιχεία επικοινωνίας θα δημοσιευθούν εδώ πριν από τη δημόσια κυκλοφορία.',
  ],
  createAccount: 'Δημιουργία λογαριασμού',
  accountSubtitle:
    'Ο λογαριασμός θυμάται τη γλώσσα σας και τους τόπους που ακολουθείτε. Η ανάγνωση λειτουργεί και χωρίς αυτόν.',
  nameLabel: 'Όνομα (αυτό εμφανίζεται αν γράψετε ποτέ εδώ)',
  emailLabel: 'Ηλεκτρονικό ταχυδρομείο',
  passwordLabel: 'Κωδικός',
  placesToFollow: 'Τόποι προς παρακολούθηση',
  followNote:
    'Ακολουθήστε όσους θέλετε. Η γλώσσα ανάγνωσης παραμένει ξεχωριστή ρύθμιση.',
  registrationPending:
    'Η εγγραφή ολοκληρώνεται μέσω Supabase Auth, που δεν έχει συνδεθεί ακόμη — κανένα στοιχείο δεν αποστέλλεται από αυτή τη φόρμα.',
  consentHeading: 'Συγκατάθεση — μία εγγραφή ανά σκοπό',
  consentIntro:
    'Κάθε μία αποθηκεύεται ως δική της χρονολογημένη εγγραφή. Ανακαλέστε οποιαδήποτε ανά πάσα στιγμή· η προηγούμενη εγγραφή διατηρείται, δεν αντικαθίσταται.',
  purposeLabels: {
    newsletter: 'Εβδομαδιαίο email με όσα δημοσιεύθηκαν',
    analytics: 'Ανώνυμα στατιστικά ανάγνωσης',
    product_news: 'Ειδοποίηση όταν ανοίγουν νέες υπηρεσίες στους τόπους μου',
  },
  purposeWord: 'σκοπός',
  granted: 'δόθηκε',
  notGranted: 'δεν έχει δοθεί',
  grant: 'Παραχώρηση',
  revoke: 'Ανάκληση',
  consentHistoryHeading: 'Το ιστορικό συγκαταθέσεών σας',
  current: 'τρέχουσα',
  recordCount: (count) =>
    `${count} εγγραφές, όχι διακόπτες. Η εκ νέου παραχώρηση ανοίγει νέα εγγραφή και αφήνει την παλιά να στέκει.`,
  notRecorded: 'Δεν καταγράφηκε',
  consentRecorded: 'Καταγράφηκε',
  consentGrantedNow: 'Νέα εγγραφή συγκατάθεσης δημιουργήθηκε· η προηγούμενη διατηρείται.',
  consentRevokedNow: 'Η συγκατάθεση ανακλήθηκε με ημερομηνία· η εγγραφή παραμένει.',
  addPlace: (placeName) => `Προσθήκη τόπου: ${placeName}`,
  removePlace: (placeName) => `Αφαίρεση τόπου: ${placeName}`,
  independenceLine: 'Η γλώσσα και ο τόπος δεν συνδυάζονται ποτέ σε μία ρύθμιση.',
  setupTitle: 'Ρυθμίστε την ανάγνωσή σας',
  setupSubtitle:
    'Η γλώσσα σας και οι τόποι που ακολουθείτε είναι ξεχωριστές επιλογές. Αλλάξτε οποιαδήποτε από τις δύο οποτεδήποτε.',
  axisOneLabel: 'Άξονας 1 — Διαβάζω στα',
  axisTwoLabel: 'Άξονας 2 — Ακολουθώ',
  alphaLanguagesNote:
    'Μόνο οι γλώσσες της άλφα. Τα αγγλικά υπάρχουν στο σχήμα και δεν είναι προσβάσιμα.',
  selectedCount: (count) => `Επιλεγμένοι: ${count}`,
  covers: (placeName) => `καλύπτει ${placeName}`,
  frontPagePreviewLabel: 'Πρώτη σελίδα:',
  later: 'Αργότερα',
  startReading: 'Έναρξη ανάγνωσης',
};

const DE: UiStrings = {
  tagline: 'Lokalnachrichten für die griechischen Gemeinden im Ausland.',
  readingLanguage: 'Lesesprache',
  places: 'Orte',
  editPlaces: 'Orte ändern',
  scopeLabels: { city: 'Lokal', region: 'Region', country: 'National' },
  originallyPublishedBy: 'Ursprünglich veröffentlicht von',
  provenance: 'Herkunft',
  source: 'Quelle',
  published: 'Veröffentlicht',
  attribution: 'Quellenvermerk',
  approved: 'Freigabe',
  record: 'Nachweis',
  wholeExtractTitle: 'Das ist der ganze Auszug',
  wholeExtractBody:
    'Die Lizenz mit diesem Verlag deckt eine wiedergegebene Überschrift und einen kurzen Auszug. Der Rest des Artikels bleibt, wo er geschrieben wurde.',
  continueAt: (host) => `Weiter bei ${host} ↗`,
  extractNote: 'Nur Auszug, laut Lizenz. Ein Klick zum Verlag.',
  moreFrom: (placeName) => `Mehr aus ${placeName}`,
  emptyPlaceTitle: 'Noch nichts veröffentlicht',
  emptyPlaceBody: (placeName) =>
    `Für ${placeName} wurde noch nichts veröffentlicht. Freigegebene Beiträge erscheinen hier.`,
  reassurance:
    'Jeder Beitrag stammt aus einem lizenzierten Feed, wird nur als Überschrift und Auszug wiedergegeben — nie im Volltext — und vor der Veröffentlichung von einer namentlich genannten Redaktion freigegeben.',
  notFoundTitle: 'Hier gibt es nichts.',
  notFoundBody: 'Diese Adresse führt zu keiner veröffentlichten Seite.',
  unavailableTitle: 'Die Seite ist gerade nicht erreichbar',
  unavailableBody:
    'Versuchen Sie es gleich noch einmal. Nichts ist verloren — die Beiträge erscheinen, sobald die Verbindung wieder steht.',
  fixtureNoticeTitle: 'Beispieldaten — keine veröffentlichten Beiträge',
  fixtureNoticeBody:
    'Diese Seite ist mit keiner Datenbank verbunden. Die Beiträge, die Verlage und die genannten Redaktionsnamen sind erfunden: nichts davon wurde von einem Menschen freigegeben, nichts davon ist veröffentlicht.',
  signIn: 'Anmelden',
  signInPending: 'Verfügbar mit der Registrierung',
  alphaLabel: 'Alpha',
  imprint: 'Impressum',
  privacy: 'Datenschutz',
  contact: 'Kontakt',
  legalPending: 'vor dem öffentlichen Start erforderlich',
  aboutLabel: 'Über epiloYES',
  editorialLabel: 'Redaktion',
  aboutHeading: 'Über epiloYES',
  aboutIntro:
    'epiloYES ist eine mehrsprachige Lokalzeitung für die griechischen Gemeinden im Ausland. Lesesprache und Orte sind zwei unabhängige Achsen: Sie lesen auf Griechisch oder Deutsch, über die Orte, die Sie betreffen — die eine Wahl berührt die andere nie.',
  aboutPipelineHeading: 'Wie es funktioniert',
  aboutPipelineParagraphs: [
    'Wir beobachten lizenzierte Nachrichtenquellen und übernehmen nur, was die jeweilige Lizenz erlaubt: Überschrift und kurzen Auszug, stets mit Link zum Original.',
    'Nichts wird ohne namentliche menschliche Freigabe veröffentlicht. Jeder Artikel wird von einem namentlich genannten Redaktionsmitglied freigegeben, bevor er auf irgendeiner Titelseite erscheint.',
    'Jeder Artikel trägt sein Protokoll: Auf seiner Seite sehen Sie Quelle und Freigabezeitpunkt; das vollständige Protokoll — Lizenz, Übersetzungsmodell, Prompt-Version, Freigebende — wird dauerhaft festgehalten.',
  ],
  aboutTranslationHeading: 'Maschinelle Übersetzung, menschliche Verantwortung',
  aboutTranslationParagraphs: [
    'Übersetzungen entstehen maschinell und werden vor der Veröffentlichung von einem namentlich genannten Redaktionsmitglied geprüft. Das Original ist stets neben dem Auszug verlinkt, damit Sie zur Quelle gehen können.',
  ],
  aboutCorrectionsHeading: 'Korrekturen und Rücknahmen',
  aboutCorrectionsParagraphs: [
    'Wenn wir einen Fehler machen, korrigieren wir ihn. Wird ein Artikel zurückgezogen, sagt seine Seite das offen — wir tun nicht so, als hätte es ihn nie gegeben. Das Veröffentlichungsprotokoll wird weder gelöscht noch umgeschrieben.',
  ],
  aboutPublishersHeading: 'Für Verlage',
  aboutPublishersParagraphs: [
    'Wir nutzen nur, was die jeweilige Lizenz ausdrücklich erlaubt: Auszug und Link, nie den Volltext ohne schriftliche Erlaubnis. Wenn Sie Fragen zu Ihrer Lizenz haben: Die Kontaktdaten werden hier vor dem öffentlichen Start veröffentlicht.',
  ],
  createAccount: 'Konto anlegen',
  accountSubtitle:
    'Ein Konto merkt sich Ihre Sprache und die Orte, denen Sie folgen. Lesen geht auch ohne.',
  nameLabel: 'Name (dieser Name erscheint, falls Sie hier je schreiben)',
  emailLabel: 'E-Mail',
  passwordLabel: 'Passwort',
  placesToFollow: 'Orte, denen Sie folgen',
  followNote: 'Folgen Sie so vielen, wie Sie mögen. Ihre Lesesprache bleibt eine eigene Einstellung.',
  registrationPending:
    'Die Registrierung läuft über Supabase Auth, das noch nicht angebunden ist — dieses Formular sendet nichts.',
  consentHeading: 'Einwilligung — ein Eintrag je Zweck',
  consentIntro:
    'Jede wird als eigener datierter Eintrag gespeichert. Widerrufen Sie jederzeit; der frühere Eintrag bleibt erhalten und wird nicht überschrieben.',
  purposeLabels: {
    newsletter: 'Eine wöchentliche E-Mail über Veröffentlichtes',
    analytics: 'Anonyme Lesestatistik',
    product_news: 'Benachrichtigung, wenn neue Dienste an meinen Orten öffnen',
  },
  purposeWord: 'Zweck',
  granted: 'erteilt',
  notGranted: 'nicht erteilt',
  grant: 'Erteilen',
  revoke: 'Widerrufen',
  consentHistoryHeading: 'Ihr Einwilligungsverlauf',
  current: 'aktuell',
  recordCount: (count) =>
    `${count} Einträge, keine Schalter. Eine erneute Erteilung öffnet einen neuen Eintrag und lässt den alten stehen.`,
  notRecorded: 'Nicht verzeichnet',
  consentRecorded: 'Verzeichnet',
  consentGrantedNow: 'Ein neuer Einwilligungseintrag wurde angelegt; der frühere bleibt erhalten.',
  consentRevokedNow: 'Die Einwilligung wurde datiert widerrufen; der Eintrag bleibt bestehen.',
  addPlace: (placeName) => `Ort hinzufügen: ${placeName}`,
  removePlace: (placeName) => `Ort entfernen: ${placeName}`,
  independenceLine: 'Sprache und Ort werden nie zu einer Einstellung kombiniert.',
  setupTitle: 'Richten Sie Ihr Lesen ein',
  setupSubtitle:
    'Ihre Sprache und die Orte, denen Sie folgen, sind getrennte Entscheidungen. Ändern Sie beides jederzeit.',
  axisOneLabel: 'Achse 1 — Ich lese auf',
  axisTwoLabel: 'Achse 2 — Ich folge',
  alphaLanguagesNote:
    'Nur die Alpha-Sprachen. Englisch existiert im Schema und ist nicht erreichbar.',
  selectedCount: (count) => `Ausgewählt: ${count}`,
  covers: (placeName) => `umfasst ${placeName}`,
  frontPagePreviewLabel: 'Startseite:',
  later: 'Später',
  startReading: 'Lesen starten',
};

const STRINGS: Readonly<Record<ReadingLanguage, UiStrings>> = { el: EL, de: DE };

/** The chrome strings for a reading language. */
export function uiStrings(lang: ReadingLanguage): UiStrings {
  return STRINGS[lang];
}
