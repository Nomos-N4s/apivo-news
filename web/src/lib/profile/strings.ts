import type { ReadingLanguage } from '../reader/axes';

/**
 * The member profile (issue #610).
 *
 * A small catalogue on purpose. Three labels this screen obviously needs —
 * the address, the reading language, the places — are already owned by
 * `uiStrings`, and `cashbackStrings` owns the two figures and the verified
 * payout phrase. Adding second versions here would put two labels for one
 * field in one product, which is the mistake `member/strings.ts` names in
 * its own header. So this file holds only what is new to this surface.
 *
 * WHAT THE BOARDS SAY AND THIS DOES NOT. `Profile.dc.html` and
 * `ProfileDesktop.dc.html` stamp each section with its owner — the shell's
 * or ours — and tell the reader, of the identity section, that we keep none
 * of it: the shell hands over a token and we exchange it for a session.
 *
 * That is false in this deployment. There is no shell. Sign-in is an email
 * link, the exchange is ours, and the address sits in our own `account` row
 * — so `identityNote` says that instead. Copy which tells somebody their
 * address is not held here, on a page rendering it from our database, is
 * the one sentence on this screen that would be worth complaining about.
 *
 * `payoutNone` is a third standing the boards do not draw. They show
 * verified and unverified; the state every member is actually in is having
 * no destination at all, because nothing in this repository can add one.
 * `payoutNoWayYet` says so rather than leaving a chip that looks like a
 * control.
 */
export interface ProfileStrings {
  readonly heading: string;
  readonly intro: string;

  /** The section for what signing in settled. */
  readonly identityHeading: string;
  /**
   * Where the address comes from and why it is here — the correction to
   * the boards, and the reason this module has a header.
   */
  readonly identityNote: string;
  readonly nameLabel: string;
  /** No display name was ever set; the address is all there is. */
  readonly nameUnset: string;

  /** The section for the two reading axes. */
  readonly readingHeading: string;
  /** Language and place are chosen separately (Principle VII, FR-009). */
  readonly axesNote: string;
  /** No place preference has been recorded in this browser yet. */
  readonly noPlacesYet: string;
  readonly choosePlaces: string;

  readonly payoutLabel: string;
  /** A destination exists and has not been verified. */
  readonly payoutNotVerified: string;
  /** No destination at all, which is everybody today. */
  readonly payoutNone: string;
  /** And nothing here can change that yet, so the chip is not a control. */
  readonly payoutNoWayYet: string;

  /** The section for what the money surfaces know. */
  readonly activityHeading: string;

  /** The account could not be read. About the deployment, not the member. */
  readonly accountUnavailable: string;
  /**
   * The token verifies and names an account that is gone. The only state on
   * this page with a remedy the member can carry out, so it names it.
   */
  readonly accountUnknown: string;
}

const el: ProfileStrings = {
  heading: 'Το προφίλ σου',
  intro: 'Τι ξέρουμε για σένα, και από πού.',

  identityHeading: 'Η ταυτότητά σου',
  identityNote:
    'Η διεύθυνση είναι αυτή με την οποία συνδέθηκες. Την κρατάμε εδώ γιατί χωρίς αυτήν δεν υπάρχει λογαριασμός — δεν έρχεται από κάπου αλλού.',
  nameLabel: 'Όνομα',
  nameUnset: 'Δεν έχει οριστεί',

  readingHeading: 'Η ανάγνωσή σου',
  axesNote:
    'Δύο άξονες, ποτέ ένας συνδυασμός: τη γλώσσα και τον τόπο τους διαλέγεις χωριστά.',
  noPlacesYet: 'Δεν έχεις διαλέξει ακόμη τόπους σε αυτό το πρόγραμμα περιήγησης.',
  choosePlaces: 'Διάλεξε τόπους',

  payoutLabel: 'Προορισμός πληρωμής',
  payoutNotVerified: 'Δεν έχει επαληθευτεί',
  payoutNone: 'Δεν έχει οριστεί',
  payoutNoWayYet:
    'Δεν υπάρχει ακόμη τρόπος να προσθέσεις προορισμό πληρωμής από εδώ.',

  activityHeading: 'Η δραστηριότητά σου',

  accountUnavailable:
    'Τα στοιχεία του λογαριασμού σου δεν φορτώνουν αυτή τη στιγμή. Δοκίμασε ξανά σε λίγο.',
  accountUnknown:
    'Αυτή η σύνδεση δείχνει σε λογαριασμό που δεν υπάρχει πια. Αποσυνδέσου και ξαναμπές.',
};

const de: ProfileStrings = {
  heading: 'Dein Profil',
  intro: 'Was hier über dich steht, und woher es kommt.',

  identityHeading: 'Deine Identität',
  identityNote:
    'Das ist die Adresse, mit der du dich angemeldet hast. Sie liegt hier, weil es ohne sie kein Konto gibt — von woanders kommt sie nicht.',
  nameLabel: 'Name',
  nameUnset: 'Nicht gesetzt',

  readingHeading: 'Dein Lesen',
  axesNote:
    'Zwei Achsen, nie eine Kombination: Sprache und Ort wählst du getrennt.',
  noPlacesYet: 'Du hast in diesem Browser noch keine Orte gewählt.',
  choosePlaces: 'Orte wählen',

  payoutLabel: 'Auszahlungsziel',
  payoutNotVerified: 'Nicht verifiziert',
  payoutNone: 'Nicht eingerichtet',
  payoutNoWayYet:
    'Ein Auszahlungsziel lässt sich von hier aus noch nicht hinzufügen.',

  activityHeading: 'Deine Aktivität',

  accountUnavailable:
    'Deine Kontodaten lassen sich gerade nicht laden. Versuch es gleich noch einmal.',
  accountUnknown:
    'Diese Anmeldung zeigt auf ein Konto, das es nicht mehr gibt. Melde dich ab und neu an.',
};

const CATALOGUES: Readonly<Record<ReadingLanguage, ProfileStrings>> = { el, de };

/** The profile copy for a reading language. */
export function profileStrings(lang: ReadingLanguage): ProfileStrings {
  return CATALOGUES[lang];
}
