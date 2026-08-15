import type { FrontItem } from './api';

/**
 * Development fixtures serving the reader pages until the public reader
 * endpoints land (T023/T024) — the shapes and the editorial posture of the
 * mockups, in the languages the product actually renders (FR-015).
 *
 * Publishers here are invented and live on `.example` domains: fixture
 * data must not name real publishers, because nothing in this repository
 * may imply a licence agreement that does not exist. Approver names are
 * the mockups' fictional editors.
 */
export const FRONT_FIXTURES: readonly FrontItem[] = [
  // — Greek reading language —
  {
    id: '0d9f4a12-6b1e-4c8a-9a3f-2e7c51b8d4a1',
    headline:
      'Το δημοτικό συμβούλιο ενέκρινε την επέκταση του τραμ προς το Freiham μετά από τετραετή εξέταση',
    extract:
      'Η απόφαση προσθέτει έντεκα στάσεις στο δυτικό άκρο της πόλης και δεσμεύει χρηματοδότηση έως το 2031. Οι εργασίες αναμένεται να ξεκινήσουν την άνοιξη, με το πρώτο τμήμα να ανοίγει σταδιακά.',
    lang: 'el',
    places: ['munich'],
    attribution:
      'Δημοσιεύθηκε αρχικά από το Münchner Tagblatt στις 14 Αυγούστου 2026. Απόδοση τίτλου και αποσπάσματος στα ελληνικά από το epiloYES· έγκριση: Ελένη Παπαδάκη.',
    source_url: 'https://tagblatt-muenchen.example/muenchen/tram-freiham-ausbau',
    published_at: '2026-08-14T07:12:00Z',
  },
  {
    id: '7c2e8f40-91d3-4b6a-8e5c-a04b92f31c6e',
    headline: 'Τα ενοίκια στο Sendling αυξήθηκαν 4,1% μέσα στον χρόνο',
    extract:
      'Ο δείκτης ενοικίων της πόλης φέρνει για πρώτη φορά από το 2019 τη συνοικία πάνω από τον μέσο όρο του Μονάχου.',
    lang: 'el',
    places: ['munich'],
    attribution:
      'Δημοσιεύθηκε αρχικά από τον Isar Kurier στις 14 Αυγούστου 2026. Απόδοση τίτλου και αποσπάσματος στα ελληνικά από το epiloYES· έγκριση: Ελένη Παπαδάκη.',
    source_url: 'https://isarkurier.example/muenchen/sendling-mieten-2026',
    published_at: '2026-08-14T06:47:00Z',
  },
  {
    id: '3a1b5c7d-2e4f-46a8-b9c0-d1e2f3a4b5c6',
    headline: 'Το γραφείο δημοτολογίου ανοίγει Σάββατα σε τρία υποκαταστήματα',
    extract:
      'Τα ραντεβού για εγγραφή κατοικίας και ανανέωση ταυτότητας ανοίγουν δύο εβδομάδες νωρίτερα.',
    lang: 'el',
    places: ['munich'],
    attribution:
      'Δημοσιεύθηκε αρχικά από το Bayerischer Rundblick στις 14 Αυγούστου 2026. Απόδοση τίτλου και αποσπάσματος στα ελληνικά από το epiloYES· έγκριση: Ελένη Παπαδάκη.',
    source_url: 'https://rundblick.example/muenchen/kvr-samstag-termine',
    published_at: '2026-08-14T06:02:00Z',
  },
  {
    id: '9e8d7c6b-5a49-4382-b1a0-c9d8e7f6a5b4',
    headline: 'Η πυροσβεστική αναφέρει την πιο ήσυχη εβδομάδα της σεζόν',
    extract:
      'Οι δασικές υπηρεσίες καταγράφουν τον χαμηλότερο αριθμό εστιών από την αρχή του καλοκαιριού.',
    lang: 'el',
    places: ['greece'],
    attribution:
      'Δημοσιεύθηκε αρχικά από τα Πανελλήνια Νέα στις 14 Αυγούστου 2026. Πρωτότυπο ελληνικό κείμενο (τίτλος και απόσπασμα)· έγκριση: Δήμητρα Ανδρέου.',
    source_url: 'https://panellinia-nea.example/ellada/pyrosvestiki-isyxi-evdomada',
    published_at: '2026-08-14T05:36:00Z',
  },
  {
    id: '5f4e3d2c-1b0a-4998-8776-655443322110',
    headline: 'Τα δρομολόγια των πλοίων για τις Κυκλάδες επεκτείνονται έως τον Οκτώβριο',
    extract:
      'Οι ακτοπλοϊκές εταιρείες κρατούν τα καλοκαιρινά δρομολόγια σε ισχύ έπειτα από αυξημένη ζήτηση.',
    lang: 'el',
    places: ['greece'],
    attribution:
      'Δημοσιεύθηκε αρχικά από τα Αιγαίο Νέα στις 14 Αυγούστου 2026. Πρωτότυπο ελληνικό κείμενο (τίτλος και απόσπασμα)· έγκριση: Δήμητρα Ανδρέου.',
    source_url: 'https://aigaionea.example/aktoploia/kyklades-oktovrios',
    published_at: '2026-08-14T05:20:00Z',
  },
  {
    id: '2b3c4d5e-6f70-4181-92a3-b4c5d6e7f809',
    headline: 'Η Βουλή όρισε το φθινοπωρινό χρονοδιάγραμμα για το φορολογικό νομοσχέδιο',
    extract:
      'Οι ακροάσεις των επιτροπών ξεκινούν τον Σεπτέμβριο· η κυβέρνηση αναμένει ψηφοφορία πριν από τη συζήτηση του προϋπολογισμού.',
    lang: 'el',
    places: ['greece'],
    attribution:
      'Δημοσιεύθηκε αρχικά από τον Πρωινό Τύπο στις 14 Αυγούστου 2026. Πρωτότυπο ελληνικό κείμενο (τίτλος και απόσπασμα)· έγκριση: Δήμητρα Ανδρέου.',
    source_url: 'https://proinos-typos.example/politiki/forologiko-xronodiagramma',
    published_at: '2026-08-14T04:55:00Z',
  },

  // — German reading language: the same pipeline, the other axis value —
  {
    id: 'c1d2e3f4-a5b6-4708-b19a-2b3c4d5e6f70',
    headline: 'Stadtrat billigt die Tram-Verlängerung nach Freiham nach vierjähriger Prüfung',
    extract:
      'Der Beschluss ergänzt elf Haltestellen am westlichen Stadtrand und sichert die Finanzierung bis 2031. Der Bau soll im Frühjahr beginnen.',
    lang: 'de',
    places: ['munich'],
    attribution:
      'Ursprünglich veröffentlicht vom Münchner Tagblatt am 14. August 2026. Deutscher Originaltext (Überschrift und Auszug); Freigabe: Eleni Papadaki.',
    source_url: 'https://tagblatt-muenchen.example/muenchen/tram-freiham-ausbau',
    published_at: '2026-08-14T07:12:00Z',
  },
  {
    id: '6e5d4c3b-2a19-4087-b6c5-d4e3f2a1b0c9',
    headline: 'Mieten in Sendling steigen binnen eines Jahres um 4,1 Prozent',
    extract:
      'Der Mietspiegel der Stadt sieht den Stadtteil erstmals seit 2019 über dem Münchner Durchschnitt.',
    lang: 'de',
    places: ['munich'],
    attribution:
      'Ursprünglich veröffentlicht vom Isar Kurier am 14. August 2026. Deutscher Originaltext (Überschrift und Auszug); Freigabe: Eleni Papadaki.',
    source_url: 'https://isarkurier.example/muenchen/sendling-mieten-2026',
    published_at: '2026-08-14T06:47:00Z',
  },
  {
    id: 'a9b8c7d6-e5f4-4321-80fe-dcba98765432',
    headline: 'Parlament legt den Herbstfahrplan für das Steuergesetz fest',
    extract:
      'Die Ausschussanhörungen beginnen im September; die Regierung erwartet eine Abstimmung vor der Haushaltsdebatte.',
    lang: 'de',
    places: ['greece'],
    attribution:
      'Ursprünglich veröffentlicht von Πρωινός Τύπος am 14. August 2026. Übersetzung von Überschrift und Auszug ins Deutsche durch epiloYES; Freigabe: Dimitra Andreou.',
    source_url: 'https://proinos-typos.example/politiki/forologiko-xronodiagramma',
    published_at: '2026-08-14T04:55:00Z',
  },
];
