import type { ReadingLanguage } from '../reader/axes';

/**
 * The three legal notices, in the reading language (issue #64).
 *
 * Every string here is a LABEL or a statement this service makes in its
 * own voice. Not one of them is a fact about the operator: the entity,
 * the registered address, the jurisdiction and the three support
 * addresses all come from the brand configuration, because ADR-0004 and
 * the constitution's Rebrandability section forbid a legal entity in
 * application code — and because an Impressum whose entity is a literal
 * is wrong for every deployment but one.
 *
 * That division decides what may be translated. A label is ours and is
 * written twice. A brand value is the operator's own name and address,
 * and is rendered once, verbatim, in whichever language it was
 * registered — translating a company name is how an Impressum stops
 * identifying anybody. "Impressum · TMG §5" stays untranslated in both
 * catalogues for the same reason: it cites a German statute, and a Greek
 * rendering of the citation would name no law.
 */
export interface LegalStrings {
  /** Page heading, and the `<title>` these pages carry. */
  readonly heading: string;
  /** What the page replaces — the footer's dead text (issue #64). */
  readonly intro: string;

  readonly imprintHeading: string;
  /** The statute this pane answers. Untranslated: it cites German law. */
  readonly imprintStatute: string;
  readonly entityTerm: string;
  readonly addressTerm: string;
  readonly jurisdictionTerm: string;
  readonly contactTerm: string;

  /** Shown only when the fixture brand is answering (see load.ts). */
  readonly fixtureTitle: string;
  readonly fixtureBody: string;

  readonly privacyHeading: string;
  /** "version {v}" beside the privacy document's own version. */
  readonly privacyVersion: (version: string) => string;
  readonly keepTerm: string;
  readonly keepBody: string;
  readonly whereTerm: string;
  readonly whereBody: string;
  readonly rightsTerm: string;
  readonly rightsBody: string;
  /** How a right is exercised while self-service does not exist. */
  readonly rightsHowTitle: string;
  readonly rightsHowBody: string;

  readonly contactHeading: string;
  readonly contactIntro: string;
  readonly generalTerm: string;
  readonly legalTerm: string;
  readonly privacyTerm: string;
  readonly correctionTitle: string;
  readonly correctionBody: string;
}

const EL: LegalStrings = {
  heading: 'Νομικά',
  intro:
    'Τα τρία που ο υποσέλιδος χαρακτήριζε «εκκρεμούν». Κάθε στοιχείο εδώ διαβάζεται από το αρχείο μάρκας — καμία επωνυμία, διεύθυνση ή διεύθυνση επικοινωνίας δεν είναι γραμμένη μέσα στη σελίδα.',

  imprintHeading: 'Ταυτότητα',
  imprintStatute: 'Impressum · TMG §5',
  entityTerm: 'Φορέας',
  addressTerm: 'Διεύθυνση',
  jurisdictionTerm: 'Δικαιοδοσία',
  contactTerm: 'Επικοινωνία',

  fixtureTitle: 'Γιατί δείχνει άλλο όνομα',
  fixtureBody:
    'Αυτή η ανάπτυξη δεν ονομάζει μάρκα, οπότε απαντά η μάρκα-δείγμα του αποθετηρίου. Τα στοιχεία παρακάτω ανήκουν σε εταιρεία που δεν υπάρχει και δεν αποτελούν ταυτοποίηση παρόχου.',

  privacyHeading: 'Απόρρητο',
  privacyVersion: (version) => `έκδοση ${version}`,
  keepTerm: 'Τι κρατάμε',
  keepBody:
    'Λογαριασμό, γλώσσα ανάγνωσης, τόπους, καταχωρίσεις επιστροφής χρημάτων και τις συγκαταθέσεις σου — καθεμία ως δική της χρονολογημένη εγγραφή.',
  whereTerm: 'Πού',
  whereBody: 'Στην ΕΕ. Η δικαιοδοσία είναι καρφωμένη στην ανάπτυξη, όχι επιλογή ανά χρήστη.',
  rightsTerm: 'Τα δικαιώματά σου',
  rightsBody:
    'Αντίγραφο των δεδομένων σου, διόρθωση, διαγραφή, ανάκληση κάθε συγκατάθεσης. Η ανάκληση δεν σβήνει την παλιά εγγραφή — ανοίγει νέα που την κλείνει.',
  rightsHowTitle: 'Πώς ασκείται σήμερα',
  rightsHowBody:
    'Γράψε στη διεύθυνση δεδομένων και απορρήτου παρακάτω. Ο GDPR δίνει προθεσμία ενός μήνα για την απάντηση· δεν απαιτεί κουμπί. Η αυτοεξυπηρέτηση δεν υπάρχει ακόμη και αυτή η σελίδα δεν προσποιείται ότι υπάρχει.',

  contactHeading: 'Επικοινωνία',
  contactIntro: 'Ένα σημείο ανά θέμα.',
  generalTerm: 'Γενικά',
  legalTerm: 'Νομικά',
  privacyTerm: 'Δεδομένα και απόρρητο',
  correctionTitle: 'Διόρθωση σε άρθρο',
  correctionBody:
    'Κάθε άρθρο φέρει τον επώνυμο συντάκτη που το ενέκρινε. Μια διόρθωση πηγαίνει σε αυτόν, με την αναφορά του άρθρου.',
};

const DE: LegalStrings = {
  heading: 'Rechtliches',
  intro:
    'Die drei Angaben, die der Fußbereich bisher als „erforderlich“ auswies. Jede Angabe hier stammt aus der Markenkonfiguration — kein Firmenname, keine Anschrift und keine Kontaktadresse steht in der Seite selbst.',

  imprintHeading: 'Impressum',
  imprintStatute: 'Impressum · TMG §5',
  entityTerm: 'Anbieter',
  addressTerm: 'Anschrift',
  jurisdictionTerm: 'Rechtsraum',
  contactTerm: 'Kontakt',

  fixtureTitle: 'Warum hier ein anderer Name steht',
  fixtureBody:
    'Diese Installation benennt keine Marke, daher antwortet die Beispielmarke des Repositorys. Die folgenden Angaben gehören zu einem Unternehmen, das nicht existiert, und sind keine Anbieterkennzeichnung.',

  privacyHeading: 'Datenschutz',
  privacyVersion: (version) => `Fassung ${version}`,
  keepTerm: 'Was wir speichern',
  keepBody:
    'Konto, Lesesprache, Orte, Cashback-Buchungen und Ihre Einwilligungen — jede als eigener, datierter Eintrag.',
  whereTerm: 'Wo',
  whereBody:
    'In der EU. Der Rechtsraum ist für die Installation festgelegt und nicht pro Person wählbar.',
  rightsTerm: 'Ihre Rechte',
  rightsBody:
    'Kopie Ihrer Daten, Berichtigung, Löschung, Widerruf jeder Einwilligung. Ein Widerruf löscht den alten Eintrag nicht — er eröffnet einen neuen, der ihn schließt.',
  rightsHowTitle: 'Wie Sie sie heute ausüben',
  rightsHowBody:
    'Schreiben Sie an die unten genannte Datenschutzadresse. Die DSGVO setzt eine Frist von einem Monat für die Antwort; sie verlangt keine Schaltfläche. Eine Selbstbedienung gibt es noch nicht, und diese Seite tut nicht so, als gäbe es sie.',

  contactHeading: 'Kontakt',
  contactIntro: 'Eine Adresse je Anliegen.',
  generalTerm: 'Allgemein',
  legalTerm: 'Rechtliches',
  privacyTerm: 'Daten und Datenschutz',
  correctionTitle: 'Korrektur zu einem Beitrag',
  correctionBody:
    'Jeder Beitrag nennt die Person, die ihn freigegeben hat. Eine Korrektur geht an sie, mit der Fundstelle des Beitrags.',
};

/** The legal copy for a reading language. */
export function legalStrings(lang: ReadingLanguage): LegalStrings {
  return lang === 'el' ? EL : DE;
}
