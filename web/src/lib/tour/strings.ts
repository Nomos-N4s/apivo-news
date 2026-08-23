import type { ReadingLanguage } from '../reader/axes';
import type { TourCopyKey } from './tours';

/**
 * Tour copy, in the alpha languages.
 *
 * Here rather than in the tour definition for the reason every string in
 * this product is separated from the thing that shows it: language is an
 * axis (FR-009), and `el` and `de` are the two that mount (FR-015). A tour
 * written in English would be the one surface that ignores the axis the
 * whole product is built around — and it would be read by exactly the
 * people who most need the product to be coherent, the editorial staff.
 *
 * The tone follows `editorial/strings.ts`: say what the screen does and
 * what it costs the reader if they get it wrong. A tour that only labels
 * what is already visible is decoration.
 */
export interface TourCopy {
  readonly title: string;
  readonly body: string;
}

export interface TourStrings {
  readonly steps: Readonly<Record<TourCopyKey, TourCopy>>;
  /** driver.js chrome. */
  readonly next: string;
  readonly previous: string;
  readonly done: string;
  /** The launcher in the editorial chrome, and its title when already taken. */
  readonly start: string;
  readonly restart: string;
  /** Screen-reader label for the launcher. */
  readonly startLabel: string;
}

const EL: TourStrings = {
  steps: {
    signInIntro: {
      title: 'Ο χώρος της σύνταξης',
      body: 'Αυτή η ξενάγηση δείχνει τη διαδρομή μιας είδησης, από την ουρά ελέγχου ως την έγκριση. Διαρκεί ένα λεπτό και σταματά όποτε θέλετε.',
    },
    signInForm: {
      title: 'Συνδεθείτε με τον δικό σας λογαριασμό',
      body: 'Η έγκριση καταγράφει το όνομά σας δίπλα στο άρθρο, και για αυτόν τον λόγο ο λογαριασμός δεν είναι ποτέ κοινόχρηστος.',
    },
    queueList: {
      title: 'Η ουρά ελέγχου',
      body: 'Ό,τι περιμένει έγκριση, με τα νεότερα πρώτα. Τίποτα εδώ δεν έχει δημοσιευτεί ακόμη.',
    },
    queueRow: {
      title: 'Τι λέει κάθε γραμμή',
      body: 'Ο τόπος, η πηγή, η ώρα ανάκτησης και οι γλώσσες. Αν χρειάστηκε μετάφραση, φαίνεται εδώ πριν καν ανοίξετε το άρθρο.',
    },
    reviewPane: {
      title: 'Το παράθυρο ελέγχου',
      body: 'Ανοίγοντας μια γραμμή, το άρθρο εμφανίζεται εδώ για έλεγχο πριν από την έγκριση.',
    },
    reviewOriginal: {
      title: 'Το πρωτότυπο',
      body: 'Όπως ανακτήθηκε, αμετάβλητο. Δεν το επεξεργάζεται κανείς, ούτε εσείς.',
    },
    reviewTranslation: {
      title: 'Η μετάφραση',
      body: 'Μηχανική μετάφραση. Συγκρίνετέ την με το πρωτότυπο: εγκρίνοντας, εγκρίνετε και τα δύο.',
    },
    approveForm: {
      title: 'Η έγκριση',
      body: 'Καταγράφει το όνομά σας και την ώρα. Δεν σβήνεται σιωπηλά — μένει στον έλεγχο.',
    },
    spendLedger: {
      title: 'Δαπάνη μετάφρασης',
      body: 'Πόσο κόστισε η μετάφραση αυτόν τον μήνα, σε σχέση με το όριο. Όταν εξαντληθεί, οι ειδήσεις περιμένουν αμετάφραστες.',
    },
    sourcesNav: {
      title: 'Από πού έρχονται',
      body: 'Οι πηγές τροφοδοτούν την ουρά. Εδώ βλέπετε ποιες είναι.',
    },
    sourcesAdd: {
      title: 'Προσθήκη πηγής',
      body: 'Μια νέα πηγή στέλνει ειδήσεις στην ουρά ελέγχου, ποτέ απευθείας στη δημοσίευση.',
    },
    auditTrail: {
      title: 'Ο έλεγχος',
      body: 'Κάθε έγκριση, με όνομα και ώρα. Αυτό είναι το μητρώο που δεν επινοείται.',
    },
  },
  next: 'Επόμενο',
  previous: 'Προηγούμενο',
  done: 'Τέλος',
  start: 'Ξενάγηση',
  restart: 'Ξανά από την αρχή',
  startLabel: 'Ξεκινήστε την ξενάγηση των συντακτικών οθονών',
};

const DE: TourStrings = {
  steps: {
    signInIntro: {
      title: 'Der Redaktionsbereich',
      body: 'Diese Führung zeigt den Weg einer Meldung, von der Prüfliste bis zur Freigabe. Sie dauert eine Minute und lässt sich jederzeit beenden.',
    },
    signInForm: {
      title: 'Mit dem eigenen Konto anmelden',
      body: 'Eine Freigabe verzeichnet Ihren Namen neben dem Artikel, und aus diesem Grund wird ein Konto nie geteilt.',
    },
    queueList: {
      title: 'Die Prüfliste',
      body: 'Alles, was auf Freigabe wartet, neueste zuerst. Nichts davon ist veröffentlicht.',
    },
    queueRow: {
      title: 'Was eine Zeile verrät',
      body: 'Ort, Quelle, Zeitpunkt des Abrufs und die Sprachen. Ob übersetzt wurde, steht hier, bevor Sie den Artikel öffnen.',
    },
    reviewPane: {
      title: 'Die Prüfansicht',
      body: 'Eine geöffnete Zeile erscheint hier zur Prüfung vor der Freigabe.',
    },
    reviewOriginal: {
      title: 'Das Original',
      body: 'Wie abgerufen, unverändert. Niemand bearbeitet es, auch Sie nicht.',
    },
    reviewTranslation: {
      title: 'Die Übersetzung',
      body: 'Maschinell übersetzt. Vergleichen Sie mit dem Original: Mit der Freigabe geben Sie beides frei.',
    },
    approveForm: {
      title: 'Die Freigabe',
      body: 'Sie verzeichnet Ihren Namen und die Uhrzeit. Sie verschwindet nicht stillschweigend — der Prüfpfad behält sie.',
    },
    spendLedger: {
      title: 'Übersetzungskosten',
      body: 'Was Übersetzung diesen Monat gekostet hat, gemessen am Limit. Ist es erschöpft, warten Meldungen unübersetzt.',
    },
    sourcesNav: {
      title: 'Woher die Meldungen kommen',
      body: 'Quellen speisen die Prüfliste. Hier stehen sie.',
    },
    sourcesAdd: {
      title: 'Quelle hinzufügen',
      body: 'Eine neue Quelle liefert Meldungen in die Prüfliste, nie direkt in die Veröffentlichung.',
    },
    auditTrail: {
      title: 'Der Prüfpfad',
      body: 'Jede Freigabe, mit Namen und Uhrzeit. Das ist der Nachweis, der nicht erfunden wird.',
    },
  },
  next: 'Weiter',
  previous: 'Zurück',
  done: 'Fertig',
  start: 'Führung',
  restart: 'Von vorn',
  startLabel: 'Führung durch die Redaktionsansichten starten',
};

const STRINGS: Readonly<Record<ReadingLanguage, TourStrings>> = { el: EL, de: DE };

export function tourStrings(lang: ReadingLanguage): TourStrings {
  return STRINGS[lang];
}
