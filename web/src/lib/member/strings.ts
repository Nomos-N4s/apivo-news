import type { ReadingLanguage } from '../reader/axes';

/**
 * Member sign-in (issue #529).
 *
 * The page this copy dresses does not work, and says so on its own face.
 * Supabase Auth is not wired for members: `signInWithPassword` appears once
 * in the whole repository, on the editorial sign-in, and the front page's
 * member control is a `disabled` button. The design board draws this screen
 * and then prints, on the screen, that it does not work yet — the settings
 * board files member sign-in under "named, not mocked". This module keeps
 * that promise.
 *
 * So: no copy here may describe a mechanism. There is no "check your email",
 * no "we have sent you a link", no success and no error, because no code
 * path can reach one. `strings.test.ts` asserts the catalogue carries no
 * URL, no `mailto:` and no `http`, so a working-sounding sentence cannot be
 * added without the suite noticing.
 *
 * Two names this module deliberately does not use. The shell is called by
 * what it does — "the app account" — and never by its vendor name: a product
 * name outside the brand configuration is exactly what ADR-0004 forbids, and
 * the brand lint not catching a third party's name is not permission. And
 * the email label is `uiStrings.emailLabel`, which this repository already
 * owns; a second one would put two labels for one field in one product.
 *
 * `frontPageButtonState` takes the button's label as an argument rather than
 * quoting it, so the sentence cannot go stale when that label changes — the
 * same anti-drift discipline the FAQ uses for its answers.
 */
export interface MemberStrings {
  /** What signing in changes, and what it leaves alone. */
  readonly intro: string;
  /** The shell path: a token exchanged for our own session. */
  readonly continueWithApp: string;
  readonly tokenExchangeNote: string;
  /** The divider between the two paths. */
  readonly orWord: string;
  /** The email path's action. */
  readonly sendLink: string;
  /** The heading of the panel that states what is owed. */
  readonly notWorkingTitle: string;
  /** Names the front page's disabled control by its own current label. */
  readonly frontPageButtonState: (label: string) => string;
}

const EL: MemberStrings = {
  intro:
    'Η γλώσσα ανάγνωσης και οι τόποι μένουν όπως είναι. Με τη σύνδεση προστίθενται το πορτοφόλι και οι επιστροφές χρημάτων.',
  continueWithApp: 'Συνέχεια με τον λογαριασμό της εφαρμογής',
  tokenExchangeNote:
    'Η εφαρμογή δίνει ένα διακριτικό· εδώ ανταλλάσσεται με δική μας συνεδρία. Κανένας κωδικός δεν περνά από εδώ.',
  orWord: 'ή',
  sendLink: 'Αποστολή συνδέσμου',
  notWorkingTitle: 'Δεν λειτουργεί ακόμη',
  frontPageButtonState: (label) =>
    `Σήμερα το κουμπί σύνδεσης στην αρχική σελίδα είναι απενεργοποιημένο και γράφει «${label}».`,
};

const DE: MemberStrings = {
  intro:
    'Lesesprache und Orte bleiben, wie sie sind. Mit der Anmeldung kommen die Übersicht und das Cashback dazu.',
  continueWithApp: 'Mit dem App-Konto fortfahren',
  tokenExchangeNote:
    'Die App übergibt ein Token; hier wird es gegen eine eigene Sitzung eingetauscht. Ein Passwort geht diesen Weg nie.',
  orWord: 'oder',
  sendLink: 'Link senden',
  notWorkingTitle: 'Funktioniert noch nicht',
  frontPageButtonState: (label) =>
    `Zurzeit ist die Anmeldeschaltfläche auf der Startseite deaktiviert und trägt den Hinweis „${label}“.`,
};

const CATALOGUES: Readonly<Record<ReadingLanguage, MemberStrings>> = { el: EL, de: DE };

/** Member sign-in copy for a reading language. */
export function memberStrings(lang: ReadingLanguage): MemberStrings {
  return CATALOGUES[lang];
}
