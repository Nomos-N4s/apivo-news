package ingestion

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/mmcdole/gofeed"
	"github.com/mmcdole/gofeed/atom"
)

// NormalizedItem is this module's own representation of one feed entry.
// Feed dialects (RSS 2.0, Atom, RDF) are normalised into it at the boundary
// (research D1) so nothing downstream ever sees a gofeed type.
//
// Absent feed fields stay absent: an empty Title or Author and a nil
// Published mean the feed did not provide them, and the write path stores
// them as NULL rather than inventing values (FR-002).
type NormalizedItem struct {
	// Title is the item title exactly as the feed provided it, or empty
	// when the feed omitted one.
	Title string
	// Link is where the item lives at the origin: its <link>, or its GUID
	// when the feed identifies items by permalink instead. Empty when the
	// feed offers no usable link at all - see Validate.
	Link string
	// Author is the stated author name (or address when the feed names
	// nobody but gives an address), or empty when unstated.
	Author string
	// Published is the publication time the feed declared for this entry,
	// nil when the feed declared none. An entry that states only an edit
	// time leaves this nil: an edit is not a publication, and FR-002 keeps
	// an absent field absent rather than filling it from something
	// adjacent.
	Published *time.Time
	// Body is the fullest text the feed carried for this item: the content
	// element when present, otherwise the description/summary. It is what
	// the write path stores as the retrieved evidence, markup and all.
	Body string
	// Summary is the feed's own summary/description element, or empty when
	// the feed carried none. It is the preferred input of the D9 extract
	// rule (DeriveExtract).
	Summary string
}

// Errors reporting an item that cannot become retrieval evidence. They are
// diagnoses, returned before any database work, so the poll loop can log
// precisely which item it skipped and why - rather than the write path
// failing later on an opaque NOT NULL or CHECK violation.
var (
	// ErrNoLink reports an item with neither a link nor a URL-shaped GUID.
	// FR-002 requires an origin link with the content.
	ErrNoLink = errors.New("ingestion: item has no usable link")
	// ErrNoBody reports an item carrying no text at all. Storing it would
	// record empty "evidence", which is not the full retrieved text I-2
	// requires. The matching database constraint - a not-blank CHECK on
	// source_item.raw_body - is issue #60; until it lands this check is
	// the only thing standing between an empty item and a permanent,
	// immutable row of nothing.
	ErrNoBody = errors.New("ingestion: item has no retrieved text")
)

// Validate reports why this item cannot be stored as retrieval evidence, or
// nil when it can. Callers check it before writing; the write path checks it
// too, so an unusable item can never reach the database.
func (i NormalizedItem) Validate() error {
	if i.Link == "" {
		return ErrNoLink
	}
	if strings.TrimSpace(i.Body) == "" {
		return ErrNoBody
	}
	return nil
}

// MaxFeedBytes bounds how much of a feed document ParseFeed will read. The
// parser materialises the whole document in memory, so an oversized or
// hostile response must not be allowed to exhaust the process: polling
// continues for every other source instead. Generous for real feeds, which
// run to tens of kilobytes.
const MaxFeedBytes = 8 << 20 // 8 MiB

// ErrFeedTooLarge reports a feed document exceeding MaxFeedBytes. The poll
// loop logs it and moves on; nothing is stored from a truncated read.
var ErrFeedTooLarge = errors.New("ingestion: feed exceeds the maximum accepted size")

// boundedReader passes at most MaxFeedBytes through to the parser and fails
// the read past that. Counting in place keeps the document streaming into
// the parser instead of being materialised in a second full copy first.
type boundedReader struct {
	r io.Reader
	// remaining counts one byte BEYOND the bound, which is what
	// distinguishes a document of exactly MaxFeedBytes (legal) from a
	// longer one (refused): the extra byte can only be consumed by a
	// document that exceeds the bound.
	remaining int64
	exceeded  bool
}

func (b *boundedReader) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		b.exceeded = true
		return 0, ErrFeedTooLarge
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.r.Read(p)
	b.remaining -= int64(n)
	if b.remaining <= 0 {
		b.exceeded = true
		return n, ErrFeedTooLarge
	}
	return n, err
}

// ParseFeed parses one RSS 2.0, Atom or RDF document and normalises every
// entry. The reader is consumed up to MaxFeedBytes; a longer document is
// refused with ErrFeedTooLarge rather than parsed in part. Encoding
// declarations other than UTF-8 (a reality of Greek feeds) are converted by
// the parser. A feed with no entries parses to an empty, non-nil slice.
//
// Every entry is returned, including ones that cannot be stored: the caller
// decides what to do with them after asking Validate, which names the
// problem.
func ParseFeed(r io.Reader) ([]NormalizedItem, error) {
	bounded := &boundedReader{r: r, remaining: MaxFeedBytes + 1}
	parser := gofeed.NewParser()
	// The original document answers a question the universal item cannot:
	// whether the feed really stated a publication date (see declaredDates).
	parser.KeepOriginalFeed = true
	feed, err := parser.Parse(bounded)
	if bounded.exceeded {
		// Checked ahead of err: the parser may report the truncation in
		// its own words, and the size refusal is the accurate diagnosis.
		return nil, ErrFeedTooLarge
	}
	if err != nil {
		return nil, fmt.Errorf("ingestion: parse feed: %w", err)
	}
	declared := declaredDates(feed)
	items := make([]NormalizedItem, 0, len(feed.Items))
	for i, item := range feed.Items {
		if item == nil {
			continue
		}
		normalised := normalise(feed, item)
		if declared != nil && i < len(declared) && !declared[i] {
			normalised.Published = nil
		}
		items = append(items, normalised)
	}
	return items, nil
}

// declaredDates reports, per entry position, whether the feed stated a
// publication date for that entry - or nil when the format cannot conflate
// one with anything else and the universal field can be trusted.
//
// Atom is the case that needs asking: for an entry carrying only <updated>,
// gofeed fills BOTH the parsed and the raw publication fields from that edit
// time, so neither reveals that no publication date was ever stated. The
// original entry does. An edit is not a publication, and FR-002 keeps an
// absent field absent rather than filling it from something adjacent.
func declaredDates(feed *gofeed.Feed) []bool {
	original, ok := feed.OriginalFeed().(*atom.Feed)
	if !ok || original == nil {
		return nil
	}
	declared := make([]bool, len(original.Entries))
	for i, entry := range original.Entries {
		declared[i] = entry != nil && strings.TrimSpace(entry.Published) != ""
	}
	return declared
}

// normalise maps one gofeed item into the module's own type. Surrounding
// whitespace is trimmed everywhere: it is markup artefact (indented CDATA,
// pretty-printed XML), not content, and trimming keeps the stored body - and
// therefore the database-computed content fingerprint - stable across
// cosmetic re-serialisations of the same feed.
func normalise(feed *gofeed.Feed, item *gofeed.Item) NormalizedItem {
	summary := strings.TrimSpace(item.Description)
	body := strings.TrimSpace(item.Content)
	if body == "" {
		body = summary
	}
	return NormalizedItem{
		Title:     strings.TrimSpace(item.Title),
		Link:      linkOf(item),
		Author:    authorOf(feed, item),
		Published: publishedAt(item),
		Body:      body,
		Summary:   summary,
	}
}

// publishedAt returns the entry's stated publication time, or nil. Formats
// that can conflate a publication date with an edit time are corrected by
// declaredDates against the original document.
func publishedAt(item *gofeed.Item) *time.Time {
	if strings.TrimSpace(item.Published) == "" || item.PublishedParsed == nil {
		return nil
	}
	t := *item.PublishedParsed
	return &t
}

// linkOf returns where the item lives at the origin. Feeds that identify
// items by permalink GUID instead of <link> are common enough that ignoring
// the GUID would send otherwise complete items to the write path with no
// origin link; a GUID is only trusted when it is an absolute http(s) URL,
// since the same element is also used for opaque ids like "urn:uuid:...".
func linkOf(item *gofeed.Item) string {
	if link := strings.TrimSpace(item.Link); link != "" {
		return link
	}
	for _, alt := range item.Links {
		if link := strings.TrimSpace(alt); link != "" {
			return link
		}
	}
	if guid := strings.TrimSpace(item.GUID); isAbsoluteHTTPURL(guid) {
		return guid
	}
	return ""
}

func isAbsoluteHTTPURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// authorOf returns the author the feed states, preferring the most specific
// and most human statement available:
//
//  1. the item's Dublin Core creator - an RSS item carrying both a bare
//     <author> address and a <dc:creator> name states the name there, and
//     reading the address first would store it and drop the name;
//  2. the item's own author elements, name before address;
//  3. the feed-level author, which is where a newsroom Atom feed commonly
//     declares authorship once for every entry.
//
// Empty only when the feed names nobody anywhere, which the write path
// stores as NULL (FR-002).
func authorOf(feed *gofeed.Feed, item *gofeed.Item) string {
	if name := dublinCoreCreator(item); name != "" {
		return name
	}
	if name := firstPerson(item.Authors); name != "" {
		return name
	}
	if feed != nil {
		return firstPerson(feed.Authors)
	}
	return ""
}

func dublinCoreCreator(item *gofeed.Item) string {
	if item.DublinCoreExt == nil {
		return ""
	}
	for _, creator := range item.DublinCoreExt.Creator {
		if name := strings.TrimSpace(creator); name != "" {
			return name
		}
	}
	return ""
}

// firstPerson returns the first stated person: the name when given, the
// address when that is all the feed offers.
func firstPerson(people []*gofeed.Person) string {
	for _, person := range people {
		if person == nil {
			continue
		}
		if name := strings.TrimSpace(person.Name); name != "" {
			return name
		}
		if email := strings.TrimSpace(person.Email); email != "" {
			return email
		}
	}
	return ""
}
