// Package brand loads the one brand configuration a deployment runs
// under: the name members read, the entity behind it, its domains,
// support addresses, assets, design tokens, defaults, payout descriptor
// and per-product feature flags.
//
// It is a platform primitive and knows nothing about the business
// modules. Two rules from ADR-0004 shape the whole package:
//
//   - There is NO package-level brand. Nothing here is settable, nothing
//     is cached in a variable, and no other package may reach for "the
//     current brand" — the composition root loads a Brand once and passes
//     the value down. That single seam is what makes a per-request brand
//     resolver a local change on the day simultaneous multi-tenancy is
//     actually wanted, rather than a rewrite.
//   - There is no partial brand. Load either returns a configuration that
//     satisfies every rule in Validate, or it returns an error naming
//     every rule it broke. A brand that is half filled in renders a
//     surface that is half rebranded, which is worse than one that does
//     not start.
//
// The same file this package reads is read by the TypeScript loader in
// web/src/lib/brand — one source, two readers. The schema is defined
// exactly once, here, and the TypeScript declaration is derived from
// these types (see TypeScriptTypes) rather than written a second time by
// hand.
package brand

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// FileName is the name a brand definition carries inside its directory.
// A brand is a directory rather than a lone file because it owns assets
// too; the JSON is the part this package reads.
const FileName = "brand.json"

// Document kinds every brand must publish. They are required rather than
// optional because both are legally owed by an EU-facing service that
// handles money: terms are what a member accepts, and a privacy notice is
// owed under the GDPR whether or not anyone asks for it.
const (
	// DocumentTerms is the member-facing terms of service.
	DocumentTerms = "terms"
	// DocumentPrivacy is the privacy notice.
	DocumentPrivacy = "privacy"
)

// Colour tokens every brand must define. The names are the existing
// design-system tokens (web/src/styles/modernist.css) without their
// `--color-` prefix, so a brand fills the variables the stylesheet
// already reads instead of introducing a parallel vocabulary. A brand may
// define further tokens; it may not omit these four, because every
// member-facing surface resolves text on a background from them.
const (
	// ColourBackground is the page background.
	ColourBackground = "bg"
	// ColourSurface is the raised surface behind cards and panels.
	ColourSurface = "surface"
	// ColourText is the primary ink.
	ColourText = "text"
	// ColourAccent is the brand's accent.
	ColourAccent = "accent"
)

// DescriptorMaxLength is the longest payout descriptor a brand may
// declare. Card and SEPA statement descriptors are truncated by the
// scheme, and a truncated descriptor is how a member fails to recognise
// their own money and charges it back. Twenty-two characters is the
// narrowest common limit, so it is the one enforced here.
const DescriptorMaxLength = 22

// ErrInvalid is returned by Parse, Load and LoadDir when a brand
// definition breaks one or more of the rules in Validate. The wrapped
// error names every broken rule at once: fixing a brand file one error
// per run is a poor way to spend a founder's afternoon.
var ErrInvalid = errors.New("brand: invalid configuration")

// Brand is one deployment's complete brand configuration.
//
// Every field is required. Optionality here would be an invitation to
// ship a surface that renders an empty legal entity or a blank payout
// descriptor, and the cost of the alternative — a founder having to know
// all of it before the first deploy — is the point rather than a
// side effect.
type Brand struct {
	// ID is the brand's stable machine identifier, e.g. the value that
	// will fill the brand id column ADR-0004 carries in the domain. It
	// is a slug so it is safe in a file path, a column value and a URL.
	ID string `json:"id"`
	// Name is the product name as members read it, in the brand's own
	// capitalisation. It enters copy only as an interpolated token, so
	// this value is the only place it is written down.
	Name string `json:"name"`
	// Legal is the entity behind the brand and the versioned documents
	// members are asked to accept.
	Legal Legal `json:"legal"`
	// Domains are the hosts the brand answers on.
	Domains Domains `json:"domains"`
	// Support are the addresses members and authorities write to.
	Support Support `json:"support"`
	// Assets are the brand's images, by path within the web root.
	Assets Assets `json:"assets"`
	// Theme is the colour and typography token set.
	Theme Theme `json:"theme"`
	// Defaults are the language, place and currency a member gets before
	// they have chosen anything.
	Defaults Defaults `json:"defaults"`
	// Payout is what the brand looks like on a member's bank statement.
	Payout Payout `json:"payout"`
	// Features are per-product feature flags, keyed by product and then
	// by flag. A product absent from the map has no flags on, which is
	// how a deployment runs one product of the super app without a
	// second entry saying so.
	Features map[string]map[string]bool `json:"features"`
}

// Legal identifies who stands behind the brand and which versioned
// documents members accept. Rebranding without changing these is the
// failure mode the whole configuration exists to prevent: a renamed
// product still pointing at the previous company's terms is not a new
// brand, it is a misrepresentation.
type Legal struct {
	// Entity is the registered name of the company that is liable.
	Entity string `json:"entity"`
	// Jurisdiction is the ISO 3166-1 alpha-2 country whose law governs,
	// e.g. "DE". It is upper case because that is how ISO writes it and
	// one spelling beats a normalising function.
	Jurisdiction string `json:"jurisdiction"`
	// Address is the postal address an Impressum must carry (TMG §5 for
	// a German-facing service) and a regulator writes to.
	Address string `json:"address"`
	// Documents are the brand's legal documents by kind, at least
	// DocumentTerms and DocumentPrivacy. The version is what a member's
	// recorded consent points at, so it is part of the brand rather than
	// a constant in the consent code.
	Documents map[string]Document `json:"documents"`
}

// Document is one legal document: what to look it up by, and which
// revision of it is current.
type Document struct {
	// ID is the document's stable identifier, independent of version.
	ID string `json:"id"`
	// Version is the current revision, e.g. "2026-08-24" or "1.2.0".
	// Any non-blank token is accepted: how a brand versions its own
	// terms is its lawyer's decision, not this package's.
	Version string `json:"version"`
}

// Domains are the hosts a brand answers on. Bare hosts, never URLs: a
// scheme belongs to a deployment (which is always https in production)
// and a path belongs to a route.
type Domains struct {
	// Primary is the canonical host, e.g. "example.com".
	Primary string `json:"primary"`
	// Aliases are further hosts that reach the same deployment, such as
	// a www host or a retired domain still being honoured. May be empty.
	Aliases []string `json:"aliases"`
}

// Hosts returns Primary followed by Aliases, in that order — every host
// this brand answers on, for the callers that need the whole set rather
// than the canonical one.
func (d Domains) Hosts() []string {
	hosts := make([]string, 0, len(d.Aliases)+1)
	hosts = append(hosts, d.Primary)
	hosts = append(hosts, d.Aliases...)
	return hosts
}

// Support are the addresses printed on member-facing surfaces. All three
// must sit on one of the brand's own domains: an address left behind on
// the previous brand's domain is a rebrand that looks complete and is
// not, and it is exactly the kind of thing nobody notices until a member
// replies to it.
type Support struct {
	// General is where members write about their account.
	General string `json:"general"`
	// Legal is where notices and takedown requests are served.
	Legal string `json:"legal"`
	// Privacy is the address a GDPR request goes to.
	Privacy string `json:"privacy"`
}

// Addresses returns the three support addresses in a fixed order:
// general, legal, privacy.
func (s Support) Addresses() []string {
	return []string{s.General, s.Legal, s.Privacy}
}

// Assets are the brand's images as rooted paths within the web root,
// e.g. "/brand/logo.svg". Paths rather than URLs so that a brand's
// assets are served by the deployment that serves its pages; an absolute
// URL here would be a third party quietly holding a brand hostage.
type Assets struct {
	// Logo is the primary wordmark or logo.
	Logo string `json:"logo"`
	// LogoDark is the variant for dark surfaces.
	LogoDark string `json:"logoDark"`
	// Favicon is the browser tab icon.
	Favicon string `json:"favicon"`
}

// Paths returns the three asset paths in a fixed order: logo, dark logo,
// favicon.
func (a Assets) Paths() []string {
	return []string{a.Logo, a.LogoDark, a.Favicon}
}

// Theme is the design token set a brand supplies to the stylesheet.
type Theme struct {
	// Colours map token names to CSS colours. The four names in the
	// Colour* constants are required; further tokens are allowed and
	// are passed through to the stylesheet untouched.
	//
	// Values are lower-case six-digit hex. One spelling is enforced
	// because the brand-literal lint greps for these strings, and a
	// palette that may be written three ways is a palette the lint can
	// only find one way.
	Colours map[string]string `json:"colours"`
	// Typography is the brand's type.
	Typography Typography `json:"typography"`
}

// Typography is the brand's type: two font stacks and the weight
// headings are set in.
type Typography struct {
	// Heading is the CSS font stack for headings, including fallbacks.
	Heading string `json:"heading"`
	// Body is the CSS font stack for body text, including fallbacks.
	Body string `json:"body"`
	// HeadingWeight is the CSS weight headings are set in, 1 to 1000.
	HeadingWeight int `json:"headingWeight"`
}

// Defaults are what a member gets before they have chosen anything.
// Language and place are independent axes (constitution VII) and stay
// independent here: a brand may default to Greek in Munich without
// either value implying the other.
type Defaults struct {
	// Language is a BCP-47 primary language subtag, e.g. "el". The
	// primary subtag only: the catalogues are keyed by it, and a region
	// here would key nothing.
	Language string `json:"language"`
	// Place is the place slug a member lands on, e.g. "munich".
	Place string `json:"place"`
	// Currency is the ISO 4217 alphabetic code the brand keeps its
	// money in, e.g. "EUR".
	Currency string `json:"currency"`
}

// Payout is what the brand looks like where a member actually meets it:
// the line on their bank statement next to the money.
type Payout struct {
	// Descriptor is the statement text, at most DescriptorMaxLength
	// printable ASCII characters. ASCII because the schemes transliterate
	// anything else, and a transliterated Greek brand name is not a name
	// a member recognises.
	Descriptor string `json:"descriptor"`
}

// Feature reports whether flag is on for product. An unknown product or
// an unknown flag is off: a flag nobody has declared has not been turned
// on, and defaulting the other way would enable behaviour by typo.
func (b Brand) Feature(product, flag string) bool {
	return b.Features[product][flag]
}

// Document returns the brand's document of the given kind and whether it
// has one. DocumentTerms and DocumentPrivacy are always present in a
// validated brand; other kinds may or may not be.
func (b Brand) Document(kind string) (Document, bool) {
	doc, ok := b.Legal.Documents[kind]
	return doc, ok
}

var (
	slugPattern     = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	hostPattern     = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)
	colourPattern   = regexp.MustCompile(`^#[0-9a-f]{6}$`)
	languagePattern = regexp.MustCompile(`^[a-z]{2,3}$`)
	currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)
	countryPattern  = regexp.MustCompile(`^[A-Z]{2}$`)
	assetPattern    = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)
	addressPattern  = regexp.MustCompile(`^[A-Za-z0-9._%+-]+@([A-Za-z0-9.-]+)$`)
)

// Parse decodes and validates a brand definition. Unknown fields are
// rejected: a misspelled key in a brand file is a value that silently
// does not apply, and the surface it should have changed keeps the
// previous brand's.
func Parse(data []byte) (Brand, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()

	var b Brand
	if err := decoder.Decode(&b); err != nil {
		return Brand{}, fmt.Errorf("brand: decode: %w", err)
	}
	// Everything after the first document must be whitespace, and only a
	// second Decode proves it - the same check, for the same reason, as
	// decodeJSON in internal/editorial/httputil.go and the tour payload
	// reader in internal/account/tours.go. Decoder.More() answers a
	// narrower question ("is another VALUE coming?"), so a stray closing
	// delimiter after a valid object reads as "no more values" and a
	// truncated or concatenated brand file would be accepted. Requiring
	// io.EOF rejects both a second document and trailing syntax errors.
	if err := decoder.Decode(&json.RawMessage{}); !errors.Is(err, io.EOF) {
		return Brand{}, errors.New("brand: decode: the definition must be a single JSON document")
	}
	if err := b.Validate(); err != nil {
		return Brand{}, err
	}
	return b, nil
}

// Load reads and validates the brand definition named by name from fsys.
// It is the seam the rest of the package is built on: tests hand it a
// testdata directory, a deployment hands it the real one, and neither
// path is special.
func Load(fsys fs.FS, name string) (Brand, error) {
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return Brand{}, fmt.Errorf("brand: read %s: %w", name, err)
	}
	return Parse(data)
}

// LoadDir reads and validates the brand definition in the given
// directory — the call a composition root makes, once, at start-up.
func LoadDir(dir string) (Brand, error) {
	return Load(os.DirFS(dir), FileName)
}

// Validate reports every rule the brand breaks, in one error. A nil
// return means every member-facing surface can be rendered from this
// value without a single fallback.
func (b Brand) Validate() error {
	var problems []string
	report := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	if !slugPattern.MatchString(b.ID) {
		report("id %q is not a slug (lower-case letter first, then letters, digits or hyphens)", b.ID)
	}
	if strings.TrimSpace(b.Name) == "" {
		report("name is empty")
	} else if b.Name != strings.TrimSpace(b.Name) {
		report("name %q has leading or trailing whitespace", b.Name)
	}

	b.Legal.validate(report)
	b.Domains.validate(report)
	b.Support.validate(b.Domains.Hosts(), report)
	b.Assets.validate(report)
	b.Theme.validate(report)
	b.Defaults.validate(report)
	b.Payout.validate(report)
	validateFeatures(b.Features, report)

	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return fmt.Errorf("%w: %s", ErrInvalid, strings.Join(problems, "; "))
}

// reporter collects one broken rule. Passing it down means each type
// validates itself and the caller decides what a failure is worth.
type reporter func(format string, args ...any)

func (l Legal) validate(report reporter) {
	if strings.TrimSpace(l.Entity) == "" {
		report("legal.entity is empty")
	}
	if !countryPattern.MatchString(l.Jurisdiction) {
		report("legal.jurisdiction %q is not an ISO 3166-1 alpha-2 code", l.Jurisdiction)
	}
	if strings.TrimSpace(l.Address) == "" {
		report("legal.address is empty")
	}
	for _, kind := range []string{DocumentTerms, DocumentPrivacy} {
		if _, ok := l.Documents[kind]; !ok {
			report("legal.documents is missing %q", kind)
		}
	}
	for _, kind := range sortedKeys(l.Documents) {
		doc := l.Documents[kind]
		if strings.TrimSpace(doc.ID) == "" {
			report("legal.documents[%q].id is empty", kind)
		}
		if strings.TrimSpace(doc.Version) == "" {
			report("legal.documents[%q].version is empty", kind)
		}
	}
}

func (d Domains) validate(report reporter) {
	if !hostPattern.MatchString(d.Primary) {
		report("domains.primary %q is not a bare lower-case host name", d.Primary)
	}
	seen := map[string]bool{d.Primary: true}
	for i, alias := range d.Aliases {
		if !hostPattern.MatchString(alias) {
			report("domains.aliases[%d] %q is not a bare lower-case host name", i, alias)
			continue
		}
		if seen[alias] {
			report("domains.aliases[%d] %q is a duplicate", i, alias)
		}
		seen[alias] = true
	}
}

func (s Support) validate(hosts []string, report reporter) {
	own := make(map[string]bool, len(hosts))
	for _, host := range hosts {
		own[host] = true
	}
	addresses := s.Addresses()
	for i, field := range []string{"general", "legal", "privacy"} {
		address := addresses[i]
		match := addressPattern.FindStringSubmatch(address)
		if match == nil {
			report("support.%s %q is not an email address", field, address)
			continue
		}
		// The domain is held to the same rule as every other host in a
		// brand definition, and it is checked BEFORE ownership so the
		// error blames the right thing. A mail domain is case-insensitive
		// on the wire, but a brand file is authored and the brand-literal
		// lint greps for these strings: a domain written two ways is a
		// domain the lint can only find one way. Lower-casing it here
		// instead would hide a shouted domain rather than fix it, and
		// leave the file disagreeing with itself.
		domain := match[1]
		if !hostPattern.MatchString(domain) {
			report("support.%s domain %q is not a bare lower-case host name", field, domain)
			continue
		}
		if !own[domain] {
			report("support.%s %q is not on one of the brand's own domains", field, address)
		}
	}
}

func (a Assets) validate(report reporter) {
	paths := a.Paths()
	for i, field := range []string{"logo", "logoDark", "favicon"} {
		path := paths[i]
		if !assetPattern.MatchString(path) {
			report("assets.%s %q is not a rooted path within the web root", field, path)
			continue
		}
		if strings.Contains(path, "..") {
			report("assets.%s %q escapes the web root", field, path)
		}
	}
}

func (t Theme) validate(report reporter) {
	for _, token := range []string{ColourBackground, ColourSurface, ColourText, ColourAccent} {
		if _, ok := t.Colours[token]; !ok {
			report("theme.colours is missing %q", token)
		}
	}
	for _, token := range sortedKeys(t.Colours) {
		if !slugPattern.MatchString(token) {
			report("theme.colours key %q is not a slug", token)
		}
		if value := t.Colours[token]; !colourPattern.MatchString(value) {
			report("theme.colours[%q] %q is not a lower-case six-digit hex colour", token, value)
		}
	}
	if strings.TrimSpace(t.Typography.Heading) == "" {
		report("theme.typography.heading is empty")
	}
	if strings.TrimSpace(t.Typography.Body) == "" {
		report("theme.typography.body is empty")
	}
	if t.Typography.HeadingWeight < 1 || t.Typography.HeadingWeight > 1000 {
		report("theme.typography.headingWeight %d is outside the CSS range 1-1000", t.Typography.HeadingWeight)
	}
}

func (d Defaults) validate(report reporter) {
	if !languagePattern.MatchString(d.Language) {
		report("defaults.language %q is not a BCP-47 primary language subtag", d.Language)
	}
	if !slugPattern.MatchString(d.Place) {
		report("defaults.place %q is not a slug", d.Place)
	}
	if !currencyPattern.MatchString(d.Currency) {
		report("defaults.currency %q is not an ISO 4217 alphabetic code", d.Currency)
	}
}

func (p Payout) validate(report reporter) {
	switch {
	case p.Descriptor == "":
		report("payout.descriptor is empty")
	case len(p.Descriptor) > DescriptorMaxLength:
		report("payout.descriptor %q is longer than %d characters", p.Descriptor, DescriptorMaxLength)
	case p.Descriptor != strings.TrimSpace(p.Descriptor):
		report("payout.descriptor %q has leading or trailing whitespace", p.Descriptor)
	default:
		for _, r := range p.Descriptor {
			if r > unicode.MaxASCII || !unicode.IsPrint(r) {
				report("payout.descriptor %q contains a character the card schemes will transliterate", p.Descriptor)
				break
			}
		}
	}
}

func validateFeatures(features map[string]map[string]bool, report reporter) {
	if len(features) == 0 {
		report("features declares no product")
	}
	for _, product := range sortedKeys(features) {
		if !slugPattern.MatchString(product) {
			report("features key %q is not a slug", product)
		}
		for _, flag := range sortedKeys(features[product]) {
			if !slugPattern.MatchString(flag) {
				report("features[%q] key %q is not a slug", product, flag)
			}
		}
	}
}

// sortedKeys returns a map's keys in a stable order, so that a brand
// with several problems reports them the same way on every run.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
