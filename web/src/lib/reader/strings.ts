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
};

const STRINGS: Readonly<Record<ReadingLanguage, UiStrings>> = { el: EL, de: DE };

/** The chrome strings for a reading language. */
export function uiStrings(lang: ReadingLanguage): UiStrings {
  return STRINGS[lang];
}
