import type { ReadingLanguage } from '../reader/axes';

/**
 * Member sign-in (issues #529, #564).
 *
 * The page this copy dresses used to work in neither of the two directions
 * the design board draws, and said so on its own face. One of them works
 * now: an email link, and never a password. So this catalogue describes a
 * mechanism where it used to refuse to, and the discipline that kept it
 * honest moves rather than lapses — `strings.test.ts` no longer forbids a
 * URL, and does forbid the word for a password in either language, because
 * the board draws no password field and the shell note rules one out in
 * as many words.
 *
 * What is still owed is the OTHER path, and the panel that says so now
 * speaks for that one alone. Saying "neither of these works" beside a form
 * that works would be the same failure in the opposite direction.
 *
 * Two names this module deliberately does not use. The shell is called by
 * what it does — "the app account" — and never by its vendor name: a product
 * name outside the brand configuration is exactly what ADR-0004 forbids, and
 * the brand lint not catching a third party's name is not permission. And
 * the email label is `uiStrings.emailLabel`, which this repository already
 * owns; a second one would put two labels for one field in one product.
 *
 * `linkSent` takes the address as an argument rather than the page
 * assembling the sentence, so a translation can put it where its own grammar
 * wants it.
 */
export interface MemberStrings {
  /** What signing in changes, and what it leaves alone. */
  readonly intro: string;
  /** The shell path: a token exchanged for our own session. */
  readonly continueWithApp: string;
  readonly tokenExchangeNote: string;
  /** The divider between the two paths. */
  readonly orWord: string;
  /** The email path's action, and what pressing it actually does. */
  readonly sendLink: string;
  readonly emailPathNote: string;
  /**
   * The link is on its way. Named by the address it went to, because a
   * member who mistyped it will read this sentence looking for that.
   */
  readonly linkSent: (email: string) => string;
  /**
   * Which browser to open it in. Not a nicety: the exchange is completed
   * against a verifier this browser holds, so a link opened elsewhere
   * cannot finish, and a member told nothing would try exactly that.
   */
  readonly linkSentNote: string;
  /** The provider refused or could not be reached. */
  readonly sendRefused: string;
  /** This deployment has no auth configured at all. */
  readonly notConfigured: string;
  /** The link was too old, or has already been spent. */
  readonly linkExpired: string;
  /**
   * Somebody who is already signed in, on the page for signing in.
   *
   * They arrive here because the chrome on a reader page cannot tell — it
   * resolves no session, deliberately, since that costs a round trip on the
   * pages that carry most of this site's traffic. So this page has to be the
   * one that knows, and saying nothing while quietly sending them back is
   * indistinguishable from a control that does not work (#583).
   */
  readonly alreadySignedIn: (email: string) => string;
  readonly continueOnward: string;
  /** How somebody signs in as somebody else. */
  readonly signOut: string;
  readonly signedOut: string;
  /**
   * Signed in, and the address is already another account's here. The one
   * refusal on this page a member can actually act on, so it says what to
   * try instead rather than only what went wrong.
   */
  readonly addressTaken: string;
  /**
   * Signed in, and no account could be made. About the deployment rather
   * than about them, so it asks for patience and promises nothing.
   */
  readonly accountNotMade: string;
  /** The heading of the panel that states what is owed. */
  readonly notWorkingTitle: string;
  /**
   * What is owed, under that heading — now the shell path alone. Standing
   * beside a form that works and saying neither path does would be the same
   * dishonesty this panel was written to prevent, pointing the other way.
   */
  readonly notWorkingBody: string;
}

const EL: MemberStrings = {
  intro:
    'Η γλώσσα ανάγνωσης και οι τόποι μένουν όπως είναι. Με τη σύνδεση προστίθενται το πορτοφόλι και οι επιστροφές χρημάτων.',
  continueWithApp: 'Συνέχεια με τον λογαριασμό της εφαρμογής',
  tokenExchangeNote:
    'Η εφαρμογή δίνει ένα διακριτικό· εδώ ανταλλάσσεται με δική μας συνεδρία. Κανένας κωδικός δεν περνά από εδώ.',
  orWord: 'ή',
  sendLink: 'Αποστολή συνδέσμου',
  emailPathNote:
    'Στέλνουμε έναν σύνδεσμο. Ανοίγοντάς τον, συνδέεσαι — χωρίς κωδικό, ούτε τώρα ούτε αργότερα.',
  linkSent: (email) => `Ο σύνδεσμος στάλθηκε στο ${email}.`,
  linkSentNote:
    'Άνοιξέ τον στην ίδια συσκευή και στον ίδιο περιηγητή που τον ζήτησε. Λήγει σύντομα.',
  sendRefused: 'Ο σύνδεσμος δεν στάλθηκε. Δοκίμασε ξανά σε λίγο.',
  notConfigured:
    'Αυτή η εγκατάσταση δεν έχει ρυθμισμένη σύνδεση, οπότε δεν αποστέλλεται τίποτα.',
  linkExpired: 'Ο σύνδεσμος έληξε ή έχει ήδη χρησιμοποιηθεί. Ζήτησε καινούριο.',
  alreadySignedIn: (email) => `Είσαι ήδη συνδεδεμένος ως ${email}.`,
  continueOnward: 'Συνέχεια',
  signOut: 'Αποσύνδεση',
  signedOut: 'Αποσυνδέθηκες.',
  addressTaken:
    'Αυτή η διεύθυνση ανήκει ήδη σε άλλον λογαριασμό εδώ. Δοκίμασε τη διεύθυνση με την οποία συνδέθηκες την πρώτη φορά.',
  accountNotMade:
    'Η σύνδεση πέτυχε, αλλά δεν μπόρεσε να δημιουργηθεί λογαριασμός. Δοκίμασε ξανά αργότερα.',
  notWorkingTitle: 'Δεν λειτουργεί ακόμη',
  notWorkingBody:
    'Η διαδρομή μέσω της εφαρμογής δεν είναι συνδεδεμένη ακόμη· το κουμπί της είναι απενεργοποιημένο. Η αποστολή συνδέσμου λειτουργεί.',
};

const DE: MemberStrings = {
  intro:
    'Lesesprache und Orte bleiben, wie sie sind. Mit der Anmeldung kommen die Übersicht und das Cashback dazu.',
  continueWithApp: 'Mit dem App-Konto fortfahren',
  tokenExchangeNote:
    'Die App übergibt ein Token; hier wird es gegen eine eigene Sitzung eingetauscht. Ein Passwort geht diesen Weg nie.',
  orWord: 'oder',
  sendLink: 'Link senden',
  emailPathNote:
    'Wir schicken einen Link. Wer ihn öffnet, ist angemeldet — ohne Passwort, jetzt nicht und später auch nicht.',
  linkSent: (email) => `Der Link ist unterwegs an ${email}.`,
  linkSentNote:
    'Öffne ihn auf demselben Gerät und im selben Browser, der ihn angefordert hat. Er läuft bald ab.',
  sendRefused: 'Der Link wurde nicht verschickt. Versuch es gleich noch einmal.',
  notConfigured:
    'Für diese Installation ist keine Anmeldung eingerichtet; es wird nichts verschickt.',
  linkExpired: 'Dieser Link ist abgelaufen oder wurde schon benutzt. Fordere einen neuen an.',
  alreadySignedIn: (email) => `Du bist schon als ${email} angemeldet.`,
  continueOnward: 'Weiter',
  signOut: 'Abmelden',
  signedOut: 'Du bist abgemeldet.',
  addressTaken:
    'Diese Adresse gehört hier bereits zu einem anderen Konto. Versuch es mit der Adresse, mit der du dich zuerst angemeldet hast.',
  accountNotMade:
    'Die Anmeldung hat geklappt, aber es konnte kein Konto angelegt werden. Versuch es später noch einmal.',
  notWorkingTitle: 'Funktioniert noch nicht',
  notWorkingBody:
    'Der Weg über die App ist noch nicht angebunden; seine Schaltfläche ist deaktiviert. Der Link-Versand funktioniert.',
};

const CATALOGUES: Readonly<Record<ReadingLanguage, MemberStrings>> = { el: EL, de: DE };

/** Member sign-in copy for a reading language. */
export function memberStrings(lang: ReadingLanguage): MemberStrings {
  return CATALOGUES[lang];
}
