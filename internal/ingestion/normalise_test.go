package ingestion_test

// Parsing is tested against embedded fixtures shaped like real feeds: the
// RSS 2.0 / Atom / RDF dialects, CDATA bodies, HTML entities, a legacy
// ISO-8859-7 encoding, items missing optional fields, and an empty feed.
// No test touches the network (research D1).

import (
	"embed"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/Nomos-N4s/apivo-news/internal/ingestion"
)

// padding emits filler bytes forever, so an oversized document can be
// streamed at the parser without the test holding one in memory.
type padding struct{}

func (padding) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

//go:embed testdata/*.xml
var fixtures embed.FS

// ts parses an RFC 3339 timestamp into the pointer form NormalizedItem uses.
func ts(t *testing.T, value string) *time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("bad test timestamp %q: %v", value, err)
	}
	return &parsed
}

func equalTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

func TestParseFeedNormalisesRealWorldShapes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		file string
		want []ingestion.NormalizedItem
	}{
		{
			name: "rss2 with cdata, entities and both author styles",
			file: "testdata/rss2.xml",
			want: []ingestion.NormalizedItem{
				{
					// &#8211; decodes to an en dash.
					Title:     "Νέο πάρκο στο κέντρο – εγκαίνια τον Μάιο",
					Link:      "https://news.example.test/articles/parko",
					Author:    "Μαρία Παπαδοπούλου",
					Published: ts(t, "2025-06-02T09:30:00+03:00"),
					Body:      "<p>Το νέο πάρκο στο κέντρο της πόλης ανοίγει τον Μάιο, με παιδική χαρά και ποδηλατόδρομο.</p>",
					Summary:   "Σύντομη περίληψη: το νέο πάρκο ανοίγει τον Μάιο.",
				},
				{
					Title: "Stadtrat beschließt neue Radwege",
					Link:  "https://news.example.test/articles/radwege",
					// RSS "address (name)" author style: the name is what
					// we surface.
					Author:    "Redaktion",
					Published: ts(t, "2025-06-03T08:00:00+02:00"),
					Body:      "Der Stadtrat hat den Ausbau der Radwege beschlossen.",
					Summary:   "Der Stadtrat hat den Ausbau der Radwege beschlossen.",
				},
				{
					Title: "Bare author address alongside a named creator",
					Link:  "https://news.example.test/articles/creator",
					// dc:creator names the human; <author> carries only an
					// address. The name wins - storing the address would
					// drop the very thing the feed stated.
					Author:    "Ελένη Δημητρίου",
					Published: ts(t, "2025-06-04T07:15:00+03:00"),
					Body:      "Ο συντάκτης δηλώνεται και ως διεύθυνση και ως όνομα.",
					Summary:   "Ο συντάκτης δηλώνεται και ως διεύθυνση και ως όνομα.",
				},
			},
		},
		{
			name: "atom with html content and an updated-only entry",
			file: "testdata/atom.xml",
			want: []ingestion.NormalizedItem{
				{
					Title:     "Neues Kulturzentrum eröffnet",
					Link:      "https://atom.example.test/articles/kultur",
					Author:    "Anna Beispiel",
					Published: ts(t, "2025-06-04T10:15:00Z"),
					Body:      "<p>Das neue Kulturzentrum wurde am Mittwoch feierlich eröffnet.</p>",
					Summary:   "Das neue Kulturzentrum ist eröffnet.",
				},
				{
					Title: "Nur aktualisiert, nie veröffentlicht",
					Link:  "https://atom.example.test/articles/zwei",
					// The entry declares no <author>, so the feed-level
					// author stands in.
					Author: "Beispiel Redaktion",
					// The entry states only <updated>. An edit time is not
					// a publication date, so the field stays absent
					// (FR-002) even though the parser offers the timestamp.
					Published: nil,
					Body:      "Nur eine Zusammenfassung, kein Volltext.",
					Summary:   "Nur eine Zusammenfassung, kein Volltext.",
				},
				{
					Title: "Nur ein self-Link",
					// No alternate link: the self link is the only URL the
					// entry offers, and it is still where the item lives.
					Link:      "https://atom.example.test/articles/drei",
					Author:    "Beispiel Redaktion",
					Published: ts(t, "2025-06-07T06:00:00Z"),
					Body:      "Kein alternate-Link, nur self.",
					Summary:   "Kein alternate-Link, nur self.",
				},
			},
		},
		{
			name: "rdf with dublin core creator and date",
			file: "testdata/rdf.xml",
			want: []ingestion.NormalizedItem{
				{
					Title:     "Παλαιό μορφότυπο, νέο περιεχόμενο",
					Link:      "https://rdf.example.test/articles/a1",
					Author:    "Γιώργος Οικονόμου",
					Published: ts(t, "2025-06-01T07:45:00Z"),
					Body:      "Η ροή RDF εξακολουθεί να υπάρχει και πρέπει να διαβάζεται.",
					Summary:   "Η ροή RDF εξακολουθεί να υπάρχει και πρέπει να διαβάζεται.",
				},
			},
		},
		{
			name: "missing title, author and date stay absent",
			file: "testdata/rss2_sparse.xml",
			want: []ingestion.NormalizedItem{
				{
					Link:    "https://sparse.example.test/articles/no-title",
					Body:    "Χωρίς τίτλο, χωρίς συγγραφέα, χωρίς ημερομηνία.",
					Summary: "Χωρίς τίτλο, χωρίς συγγραφέα, χωρίς ημερομηνία.",
				},
				{
					Title: "Only content, no description",
					Link:  "https://sparse.example.test/articles/content-only",
					Body:  "<p>Body from content:encoded only.</p>",
					// No description element: there is no feed summary.
					Summary: "",
				},
				{
					Title: "Author gives only an address",
					Link:  "https://sparse.example.test/articles/email-only",
					// A nameless author surfaces as the stated address.
					Author:  "anonym@example.test",
					Body:    "Kein Name, nur eine Adresse.",
					Summary: "Kein Name, nur eine Adresse.",
				},
				{
					Title: "Identified by permalink GUID, no link element",
					// No <link>: the URL-shaped GUID is the origin link.
					Link:    "https://sparse.example.test/articles/by-guid",
					Body:    "Η ροή ταυτοποιεί τα άρθρα με GUID permalink.",
					Summary: "Η ροή ταυτοποιεί τα άρθρα με GUID permalink.",
				},
				{
					Title: "Opaque GUID is not a link",
					// A urn: identifier is not a URL, so there is no link.
					Link:    "",
					Body:    "Ein undurchsichtiger Bezeichner ist kein Link.",
					Summary: "Ein undurchsichtiger Bezeichner ist kein Link.",
				},
				{
					Title:   "No text at all",
					Link:    "https://sparse.example.test/articles/no-text",
					Body:    "",
					Summary: "",
				},
			},
		},
		{
			name: "legacy iso-8859-7 encoding is converted",
			file: "testdata/rss2_iso8859_7.xml",
			want: []ingestion.NormalizedItem{
				{
					Title:   "Νέα",
					Link:    "https://legacy.example.test/articles/nea",
					Body:    "Ελλάδα",
					Summary: "Ελλάδα",
				},
			},
		},
		{
			name: "empty feed parses to zero items",
			file: "testdata/rss2_empty.xml",
			want: []ingestion.NormalizedItem{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f, err := fixtures.Open(tt.file)
			if err != nil {
				t.Fatalf("opening fixture: %v", err)
			}
			defer func() { _ = f.Close() }()

			got, err := ingestion.ParseFeed(f)
			if err != nil {
				t.Fatalf("ParseFeed(%s) error: %v", tt.file, err)
			}
			if got == nil {
				t.Fatal("ParseFeed() = nil slice, want non-nil")
			}
			if len(got) != len(tt.want) {
				t.Fatalf("ParseFeed() yielded %d items, want %d: %+v", len(got), len(tt.want), got)
			}
			for i, want := range tt.want {
				item := got[i]
				if item.Title != want.Title {
					t.Errorf("item %d Title = %q, want %q", i, item.Title, want.Title)
				}
				if item.Link != want.Link {
					t.Errorf("item %d Link = %q, want %q", i, item.Link, want.Link)
				}
				if item.Author != want.Author {
					t.Errorf("item %d Author = %q, want %q", i, item.Author, want.Author)
				}
				if !equalTime(item.Published, want.Published) {
					t.Errorf("item %d Published = %v, want %v", i, item.Published, want.Published)
				}
				if item.Body != want.Body {
					t.Errorf("item %d Body = %q, want %q", i, item.Body, want.Body)
				}
				if item.Summary != want.Summary {
					t.Errorf("item %d Summary = %q, want %q", i, item.Summary, want.Summary)
				}
			}
		})
	}
}

// TestParseFeedBoundsDocumentSize asserts the parser refuses an oversized
// document outright: the whole feed is materialised in memory, so an
// unbounded read would let one hostile or runaway source exhaust the
// process that is polling every other source.
func TestParseFeedBoundsDocumentSize(t *testing.T) {
	t.Parallel()

	// A valid feed padded past the bound with comment bytes, streamed
	// rather than materialised: the refusal must come from the size check,
	// not from a parse failure, and the test must not itself hold a second
	// copy of an oversized document in memory.
	oversized := io.MultiReader(
		strings.NewReader(`<?xml version="1.0"?><rss version="2.0"><channel><title>t</title><!--`),
		io.LimitReader(padding{}, ingestion.MaxFeedBytes),
		strings.NewReader(`--><item><title>i</title><link>https://example.test/i</link></item></channel></rss>`),
	)

	if _, err := ingestion.ParseFeed(oversized); !errors.Is(err, ingestion.ErrFeedTooLarge) {
		t.Fatalf("ParseFeed(oversized) error = %v, want ErrFeedTooLarge", err)
	}

	// A document of EXACTLY MaxFeedBytes is legal: the bound counts one
	// byte beyond itself precisely so the limit is inclusive, and a feed
	// that happens to land on it must still be ingested.
	prefix := `<?xml version="1.0"?><rss version="2.0"><channel><title>t</title><!--`
	suffix := `--><item><title>i</title><link>https://example.test/i</link></item></channel></rss>`
	atLimit := io.MultiReader(
		strings.NewReader(prefix),
		io.LimitReader(padding{}, int64(ingestion.MaxFeedBytes-len(prefix)-len(suffix))),
		strings.NewReader(suffix),
	)
	items, err := ingestion.ParseFeed(atLimit)
	if err != nil {
		t.Fatalf("ParseFeed(exactly MaxFeedBytes) error = %v, want success", err)
	}
	if len(items) != 1 {
		t.Fatalf("ParseFeed(exactly MaxFeedBytes) returned %d items, want 1", len(items))
	}

	// A feed inside the bound still parses: the guard must not reject
	// legitimate feeds.
	items, err = ingestion.ParseFeed(strings.NewReader(
		`<?xml version="1.0"?><rss version="2.0"><channel><title>t</title>` +
			`<item><title>i</title><link>https://example.test/i</link></item></channel></rss>`))
	if err != nil {
		t.Fatalf("ParseFeed(small feed): %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("ParseFeed(small feed) returned %d items, want 1", len(items))
	}
}

// TestNormalizedItemValidate pins the explicit outcomes for items that
// cannot become retrieval evidence. Without them the write path would fail
// on a NOT NULL or CHECK violation, reporting that a constraint was hit but
// not which item was unusable or why.
func TestNormalizedItemValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		item ingestion.NormalizedItem
		want error
	}{
		{
			name: "complete item is usable",
			item: ingestion.NormalizedItem{Link: "https://example.test/a", Body: "text"},
			want: nil,
		},
		{
			name: "no link",
			item: ingestion.NormalizedItem{Body: "text"},
			want: ingestion.ErrNoLink,
		},
		{
			name: "no body",
			item: ingestion.NormalizedItem{Link: "https://example.test/a"},
			want: ingestion.ErrNoBody,
		},
		{
			name: "blank body is no body",
			item: ingestion.NormalizedItem{Link: "https://example.test/a", Body: "  \n\t "},
			want: ingestion.ErrNoBody,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := tt.item.Validate(); !errors.Is(err, tt.want) {
				t.Fatalf("Validate() = %v, want %v", err, tt.want)
			}
		})
	}
}

// TestParsedItemsCarryTheirVerdict walks the sparse fixture end to end: the
// two unusable items must be diagnosable by name rather than reaching the
// database and failing there.
func TestParsedItemsCarryTheirVerdict(t *testing.T) {
	t.Parallel()

	f, err := fixtures.Open("testdata/rss2_sparse.xml")
	if err != nil {
		t.Fatalf("opening fixture: %v", err)
	}
	defer func() { _ = f.Close() }()

	items, err := ingestion.ParseFeed(f)
	if err != nil {
		t.Fatalf("ParseFeed() error: %v", err)
	}

	verdicts := make(map[string]error, len(items))
	for _, item := range items {
		verdicts[item.Title] = item.Validate()
	}
	if err := verdicts["Opaque GUID is not a link"]; !errors.Is(err, ingestion.ErrNoLink) {
		t.Errorf("opaque-GUID item Validate() = %v, want ErrNoLink", err)
	}
	if err := verdicts["No text at all"]; !errors.Is(err, ingestion.ErrNoBody) {
		t.Errorf("text-free item Validate() = %v, want ErrNoBody", err)
	}
	if err := verdicts["Identified by permalink GUID, no link element"]; err != nil {
		t.Errorf("permalink-GUID item Validate() = %v, want nil", err)
	}
}

func TestParseFeedRejectsNonFeeds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
	}{
		{name: "not xml", input: "definitely not a feed"},
		{name: "xml but not a feed", input: "<html><body>hello</body></html>"},
		{name: "empty input", input: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := ingestion.ParseFeed(strings.NewReader(tt.input)); err == nil {
				t.Fatalf("ParseFeed(%q): want error, got nil", tt.input)
			}
		})
	}
}
