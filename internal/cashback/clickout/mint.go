package clickout

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
)

// entropyBytes is how much randomness one click reference carries: 16 bytes,
// which is the 128 bits FR-020 specifies, exactly.
//
// Not a round number chosen for comfort. base64url encodes 16 bytes as 22
// characters with no padding, and 22 is precisely what
// click_ref_url_safe_and_long_enough requires (migration 0012) - so the
// entropy the requirement names and the length the schema checks are the
// same fact seen twice, rather than two numbers that could drift apart.
//
// More would not buy anything a collision could use: the references are
// unique-constrained, and at 128 bits a collision across every click this
// product will ever issue stays far below the chance of the database losing
// the row some other way.
const entropyBytes = 16

// ErrNoClickReference reports that no reference could be minted, because the
// entropy source failed or gave less than was asked for.
//
// It is a hard failure with no fallback, and that is the whole design. A
// reference minted from a weaker source is guessable, and a guessable
// reference lets somebody else's purchase be claimed as a member's own
// (FR-020) - so a click-out that cannot be minted must not happen at all.
// The nearest miss worth naming: a source that returns fewer bytes than
// asked would otherwise yield a SHORTER reference, which the click table
// refuses and which an attacker can enumerate.
var ErrNoClickReference = errors.New("clickout: no click reference could be minted")

// Minter mints click references. Build it with [NewMinter]; the zero value
// has no entropy source and mints nothing.
type Minter struct {
	entropy io.Reader
}

// MinterOption configures a [Minter].
type MinterOption func(*Minter)

// WithEntropy replaces the entropy source. It exists for tests - to prove
// that a failing or short source produces no reference rather than a weak
// one - and production wiring must never pass it.
func WithEntropy(r io.Reader) MinterOption {
	return func(m *Minter) { m.entropy = r }
}

// NewMinter builds a minter over crypto/rand.
func NewMinter(opts ...MinterOption) *Minter {
	m := &Minter{entropy: rand.Reader}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Mint returns a fresh click reference, or an error wrapping
// [ErrNoClickReference].
//
// The encoding is base64url WITHOUT padding, which matters twice over: the
// '=' a padded encoder appends is not in the character class the click table
// accepts, and it carries no information, so padding would make the
// reference both longer and refused. io.ReadFull rather than Read, so a
// source that returns fewer bytes than asked is a failure rather than a
// shorter - and weaker - reference.
//
// The result goes back through [networks.NewIssuedClickRef] rather than
// being wrapped directly. That is not ceremony: it is the same constructor
// the redirect and the click row are checked by, so if this encoding ever
// stopped satisfying the schema's rule, minting would fail here rather than
// at the insert, after the member has been redirected.
func (m *Minter) Mint() (networks.IssuedClickRef, error) {
	if m.entropy == nil {
		return networks.IssuedClickRef{}, fmt.Errorf("%w: the minter has no entropy source", ErrNoClickReference)
	}
	raw := make([]byte, entropyBytes)
	if _, err := io.ReadFull(m.entropy, raw); err != nil {
		return networks.IssuedClickRef{}, fmt.Errorf("%w: reading %d bytes of entropy: %w", ErrNoClickReference, entropyBytes, err)
	}
	ref, err := networks.NewIssuedClickRef(base64.RawURLEncoding.EncodeToString(raw))
	if err != nil {
		// Unreachable while entropyBytes and the encoding agree with the
		// schema. Reported rather than assumed away, because the assumption
		// is what a future change to either would quietly break.
		return networks.IssuedClickRef{}, fmt.Errorf("%w: %w", ErrNoClickReference, err)
	}
	return ref, nil
}
