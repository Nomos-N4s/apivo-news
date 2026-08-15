package ingestion

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/mmcdole/gofeed"
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
	// Link is the item's own URL: where the content lives at the origin.
	Link string
	// Author is the stated author name (or address when the feed names
	// nobody but gives an address), or empty when unstated.
	Author string
	// Published is the publication time the feed declared for this entry,
	// nil when the feed declared none. For Atom entries that state only an
	// updated time the parser reports that time - still a feed-stated
	// timestamp for the entry, never one fabricated here (FR-002).
	Published *time.Time
	// Body is the fullest text the feed carried for this item: the content
	// element when present, otherwise the description/summary. It is what
	// the write path stores as the retrieved evidence.
	Body string
	// Summary is the feed's own summary/description element, or empty when
	// the feed carried none. It is the preferred input of the D9 extract
	// rule (DeriveExtract).
	Summary string
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

// ParseFeed parses one RSS 2.0, Atom or RDF document and normalises every
// entry. The reader is consumed up to MaxFeedBytes; a longer document is
// refused with ErrFeedTooLarge rather than parsed in part. Encoding
// declarations other than UTF-8 (a reality of Greek feeds) are converted by
// the parser. A feed with no entries parses to an empty, non-nil slice.
func ParseFeed(r io.Reader) ([]NormalizedItem, error) {
	// One byte beyond the bound distinguishes "exactly at the limit" from
	// "truncated", so an oversized feed is never silently half-parsed.
	limited := io.LimitReader(r, MaxFeedBytes+1)
	document, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("ingestion: read feed: %w", err)
	}
	if len(document) > MaxFeedBytes {
		return nil, ErrFeedTooLarge
	}
	feed, err := gofeed.NewParser().Parse(bytes.NewReader(document))
	if err != nil {
		return nil, fmt.Errorf("ingestion: parse feed: %w", err)
	}
	items := make([]NormalizedItem, 0, len(feed.Items))
	for _, item := range feed.Items {
		if item == nil {
			continue
		}
		items = append(items, normalise(item))
	}
	return items, nil
}

// normalise maps one gofeed item into the module's own type. Surrounding
// whitespace is trimmed everywhere: it is markup artefact (indented CDATA,
// pretty-printed XML), not content, and trimming keeps the stored body - and
// therefore the database-computed content fingerprint - stable across
// cosmetic re-serialisations of the same feed.
func normalise(item *gofeed.Item) NormalizedItem {
	summary := strings.TrimSpace(item.Description)
	body := strings.TrimSpace(item.Content)
	if body == "" {
		body = summary
	}
	var published *time.Time
	if item.PublishedParsed != nil {
		t := *item.PublishedParsed
		published = &t
	}
	return NormalizedItem{
		Title:     strings.TrimSpace(item.Title),
		Link:      strings.TrimSpace(item.Link),
		Author:    authorOf(item),
		Published: published,
		Body:      body,
		Summary:   summary,
	}
}

// authorOf returns the first stated author: the name when given, the email
// address when the feed gives only that, empty when the feed names nobody.
func authorOf(item *gofeed.Item) string {
	for _, person := range item.Authors {
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
