import type { ReadingLanguage } from '../reader/axes';

/**
 * The member settings screen (issue #617).
 *
 * Smaller than the screen looks, because most of what it says is already
 * owned elsewhere and a second copy of a label is two labels for one field.
 * `uiStrings` owns the consent heading and its intro — its Greek is already
 * the board's own heading, "Συγκατάθεση — μία εγγραφή ανά σκοπό" — the
 * reading-language and places labels, the outcome words, and `editPlaces`,
 * which is the link to the editor this screen refuses to duplicate.
 * `profileStrings` owns the payout field's three standings and the sentence
 * about the two axes. This module holds what is genuinely new: the page's
 * own name, two section headings, and the notifications sketch.
 *
 * THE SKETCH IS THE POINT OF THIS FILE. `SettingsSeam.dc.html` puts push
 * notifications in a column headed "Neither, yet", and states the rule for
 * everything in it: *named as owed, never mocked up as working — a settings
 * screen that shows a switch for a channel that does not exist is worse than
 * one that admits it.* There is no service worker, no web-push and no channel
 * anywhere in this repository. So the copy below describes what we would ask
 * the shell for and says plainly that none of it is built; `sketchTitle`
 * carries the word for a sketch so the label itself refuses to be read as
 * shipped, and there is no control beside it to press.
 */
export interface SettingsStrings {
  readonly heading: string;
  readonly intro: string;

  /** The money section. The field label itself is `profileStrings`'. */
  readonly payoutsHeading: string;
  /** Why a destination has to be proved before money moves (FR-051). */
  readonly payoutNote: string;

  /**
   * What a preview is looking at. `uiStrings.fixtureNoticeBody` speaks of
   * invented articles, publishers and editors, which is the news pages'
   * truth and not this one's: what is invented here is a consent history,
   * and the sharper fact is that pressing a switch records nothing at all.
   */
  readonly fixtureNotice: string;

  readonly notificationsHeading: string;
  /** Marked as a sketch in the label, so it cannot read as shipped. */
  readonly sketchTitle: string;
  /** What we would own, and what the shell would own. */
  readonly sketchDivision: string;
  /** And that none of it exists here yet. */
  readonly sketchNothingYet: string;
}

const el: SettingsStrings = {
  heading: 'Ρυθμίσεις',
  intro: 'Όσα ορίζεις εδώ αφορούν την ανάγνωση και τα χρήματά σου.',

  payoutsHeading: 'Πληρωμές',
  payoutNote:
    'Η πληρωμή γίνεται μόνο σε λογαριασμό που σου ανήκει και έχει επαληθευτεί.',

  fixtureNotice:
    'Δείγμα δεδομένων. Το ιστορικό συγκαταθέσεων παρακάτω είναι επινοημένο, και τίποτα από όσα πατήσεις εδώ δεν καταγράφεται.',

  notificationsHeading: 'Ειδοποιήσεις',
  sketchTitle: 'Σκίτσο — δεν υπάρχει ακόμη κανάλι',
  sketchDivision:
    'Εμείς θα ορίζαμε ποια δικά μας γεγονότα αξίζουν αποστολή — μια καταχώριση επιβεβαιώθηκε, μια ανάληψη εγκρίθηκε. Την άδεια της συσκευής θα την κατείχε ο φλοιός.',
  sketchNothingYet:
    'Τίποτα από αυτά δεν υπάρχει σήμερα, γι’ αυτό δεν θα βρεις εδώ διακόπτη να πατήσεις.',
};

const de: SettingsStrings = {
  heading: 'Einstellungen',
  intro: 'Was du hier festlegst, betrifft dein Lesen und dein Geld.',

  payoutsHeading: 'Auszahlungen',
  payoutNote:
    'Ausgezahlt wird nur auf ein Konto, das dir gehört und bestätigt ist.',

  fixtureNotice:
    'Beispieldaten. Der Einwilligungsverlauf unten ist erfunden, und nichts, was du hier drückst, wird gespeichert.',

  notificationsHeading: 'Benachrichtigungen',
  sketchTitle: 'Skizze — es gibt noch keinen Kanal',
  sketchDivision:
    'Wir würden festlegen, welche unserer Ereignisse eine Nachricht wert sind — eine Buchung bestätigt, eine Auszahlung freigegeben. Die Erlaubnis des Geräts läge bei der Hülle.',
  sketchNothingYet:
    'Nichts davon existiert heute, deshalb findest du hier keinen Schalter.',
};

const CATALOGUES: Readonly<Record<ReadingLanguage, SettingsStrings>> = { el, de };

/** The settings copy for a reading language. */
export function settingsStrings(lang: ReadingLanguage): SettingsStrings {
  return CATALOGUES[lang];
}
