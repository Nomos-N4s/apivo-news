import type { ArticleProvenance, QueueItem, SpendLedger } from './api';

/**
 * Development fixtures for the editorial queue until the editorial
 * endpoints land (T019 #22, T020 #23) — the shapes and states of mockup
 * 1g, with the same rules as the reader fixtures: invented publishers on
 * `.example` domains (nothing here implies a licence agreement), and the
 * mockups' fictional editors.
 */
export const QUEUE_FIXTURES: readonly QueueItem[] = [
  {
    source_item_id: '9f14c0a2-51b8-4d33-9c77-0b6e2a4f81c0',
    translation_id: 'b71c4e90-2f5a-4a18-9d6b-3c8e15a7d204',
    source_name: 'Münchner Tagblatt',
    headline_original: 'Stadtrat beschließt Tram-Verlängerung nach Freiham',
    headline_translated:
      'Το δημοτικό συμβούλιο ενέκρινε την επέκταση του τραμ προς το Freiham',
    extract_translated:
      'Η απόφαση προσθέτει έντεκα στάσεις στο δυτικό άκρο της πόλης και δεσμεύει χρηματοδότηση έως το 2031. Οι εργασίες αναμένεται να ξεκινήσουν την άνοιξη.',
    retrieved_at: '2026-08-14T06:12:04Z',
    licence_snapshot: 'Feed reuse: headline + extract, attribution and link required.',
    places: ['munich'],
    source_url: 'https://tagblatt-muenchen.example/muenchen/tram-freiham-ausbau',
    extract_original:
      'Die Entscheidung umfasst elf neue Haltestellen im Westen der Stadt und sichert die Finanzierung bis 2031. Der Bau soll im Frühjahr beginnen.',
    original_author: 'Katrin Vogel',
    original_published_at: '2026-08-14T05:58:00Z',
    content_hash: '3f9c81b0a4d75e2c8f10b93a6e5d0c47a71b',
    source_lang: 'de',
    target_lang: 'el',
    model: 'translate-alpha-1',
    prompt_version: 'v4',
    cost_microusd: 4000,
  },
  {
    source_item_id: '4c81a7d5-93e2-4b60-8a1f-77d3e5c20b96',
    translation_id: 'e2a90f34-7c15-4d82-b6a9-01f3c8d54e77',
    source_name: 'Isar Kurier',
    headline_original: 'Mieten in Sendling steigen binnen eines Jahres um 4,1 Prozent',
    headline_translated: 'Τα ενοίκια στο Sendling αυξήθηκαν 4,1% μέσα στον χρόνο',
    extract_translated:
      'Ο δείκτης ενοικίων της πόλης φέρνει για πρώτη φορά από το 2019 τη συνοικία πάνω από τον μέσο όρο του Μονάχου.',
    retrieved_at: '2026-08-14T06:47:00Z',
    licence_snapshot: 'Feed reuse: headline + extract, attribution and link required.',
    places: ['munich'],
    source_url: 'https://isarkurier.example/muenchen/sendling-mieten-2026',
    extract_original:
      'Der Mietspiegel der Stadt sieht den Stadtteil erstmals seit 2019 über dem Münchner Durchschnitt.',
    original_author: null,
    original_published_at: '2026-08-14T06:30:00Z',
    content_hash: '7d20c4e19b83f6a05c2e8d41b7930fa5c3e8',
    source_lang: 'de',
    target_lang: 'el',
    model: 'translate-alpha-1',
    prompt_version: 'v4',
    cost_microusd: 3000,
  },
  {
    // Same-language origin: approval takes the source_item directly, with
    // no translation record — the contract's untranslated path.
    source_item_id: '2ba6f318-40c7-49de-95b2-6e0a7c3f18d4',
    translation_id: null,
    source_name: 'Αιγαίο Νέα',
    headline_original: 'Τα δρομολόγια των πλοίων για τις Κυκλάδες επεκτείνονται έως τον Οκτώβριο',
    headline_translated: null,
    extract_translated: null,
    retrieved_at: '2026-08-14T05:20:00Z',
    licence_snapshot: 'Feed reuse: headline + extract, attribution and link required.',
    places: ['greece'],
    source_url: 'https://aigaionea.example/aktoploia/kyklades-oktovrios',
    extract_original:
      'Οι ακτοπλοϊκές εταιρείες κρατούν τα καλοκαιρινά δρομολόγια σε ισχύ έπειτα από αυξημένη ζήτηση.',
    original_author: null,
    original_published_at: '2026-08-14T05:05:00Z',
    content_hash: '8b02fd1160ac7e4915d3b8f207c6a1e0c4e7',
    source_lang: 'el',
    target_lang: null,
    model: null,
    prompt_version: null,
    cost_microusd: null,
  },
];

/**
 * The monthly ledger. The cap is the spec's configured monthly cap ($25,
 * tasks.md T017); the mockup's €50 was illustrative.
 */
export const SPEND_FIXTURE: SpendLedger = {
  month: '2026-08',
  spent_microusd: 9_200_000,
  cap_microusd: 25_000_000,
};

/**
 * Audit traces (mockup 1h) — the `article_provenance` view's shape.
 *
 * The first is a translated Munich item; the second is an untranslated
 * Greek item, whose `translation` is null because the target locale
 * already matched. Both carry the retrieval-time snapshots, which are the
 * legal basis — never the mutable source row.
 */
export const PROVENANCE_FIXTURES: readonly ArticleProvenance[] = [
  {
    article_id: 'a41e7c92-08d5-4d1b-9d6c-1f0b7e3a55c1',
    headline: 'Το δημοτικό συμβούλιο ενέκρινε την επέκταση του τραμ προς το Freiham',
    places: ['munich'],
    source: {
      name: 'Münchner Tagblatt',
      feed_url: 'https://tagblatt-muenchen.example/rss/muenchen',
      jurisdiction: 'DE',
    },
    source_item: {
      source_url: 'https://tagblatt-muenchen.example/muenchen/tram-freiham-ausbau',
      original_title: 'Stadtrat beschließt Tram-Verlängerung nach Freiham',
      retrieved_at: '2026-08-14T06:12:04Z',
      content_hash: '3f9c81b0a4d75e2c8f10b93a6e5d0c47a71b',
      licence_snapshot:
        'Feed reuse: headline + extract, attribution and link required. Full text prohibited.',
      usage_rule_snapshot: 'extract_and_link',
      permission_evidence_snapshot: null,
      original_author: 'Katrin Vogel',
    },
    translation: {
      model: 'translate-alpha-1',
      prompt_version: 'v4',
      target_locale: 'el',
      generated_at: '2026-08-14T06:14:22Z',
      cost_microusd: 4100,
    },
    approval: {
      approver_name: 'Eleni Papadaki',
      approver_email: 'eleni@epiloyes.example',
      approved_at: '2026-08-14T06:31:09Z',
    },
    published_at: '2026-08-14T06:31:40Z',
    withdrawal: null,
    events: [
      {
        type: 'source_item.retrieved',
        occurred_at: '2026-08-14T06:12:04Z',
        detail: 'licence and usage rule snapshotted by trigger',
      },
      {
        type: 'translation.created',
        occurred_at: '2026-08-14T06:14:22Z',
        detail: 'cost recorded, monthly ledger updated',
      },
      {
        type: 'article.approved',
        occurred_at: '2026-08-14T06:31:09Z',
        detail: 'Eleni Papadaki',
      },
      {
        type: 'article.published',
        occurred_at: '2026-08-14T06:31:40Z',
        detail: 'visible on /el/munich',
      },
    ],
  },
  {
    article_id: 'd57b1f30-6c92-4a44-b8e1-95ac2f7d0e63',
    headline: 'Τα δρομολόγια των πλοίων για τις Κυκλάδες επεκτείνονται έως τον Οκτώβριο',
    places: ['greece'],
    source: {
      name: 'Αιγαίο Νέα',
      feed_url: 'https://aigaionea.example/rss/aktoploia',
      jurisdiction: 'GR',
    },
    source_item: {
      source_url: 'https://aigaionea.example/aktoploia/kyklades-oktovrios',
      original_title: 'Τα δρομολόγια των πλοίων για τις Κυκλάδες επεκτείνονται έως τον Οκτώβριο',
      retrieved_at: '2026-08-14T05:20:00Z',
      content_hash: '8b02fd1160ac7e4915d3b8f207c6a1e0c4e7',
      licence_snapshot:
        'Feed reuse: headline + extract, attribution and link required. Full text prohibited.',
      usage_rule_snapshot: 'extract_and_link',
      permission_evidence_snapshot: null,
      original_author: null,
    },
    translation: null,
    approval: {
      approver_name: 'Dimitra Andreou',
      approver_email: 'dimitra@epiloyes.example',
      approved_at: '2026-08-14T05:31:00Z',
    },
    published_at: '2026-08-14T05:31:20Z',
    withdrawal: null,
    events: [
      {
        type: 'source_item.retrieved',
        occurred_at: '2026-08-14T05:20:00Z',
        detail: 'licence and usage rule snapshotted by trigger',
      },
      {
        type: 'article.approved',
        occurred_at: '2026-08-14T05:31:00Z',
        detail: 'Dimitra Andreou',
      },
      {
        type: 'article.published',
        occurred_at: '2026-08-14T05:31:20Z',
        detail: 'visible on /el/greece',
      },
    ],
  },
];
