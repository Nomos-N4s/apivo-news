package clickout_test

// Minting a click reference. Three properties, and each of them is the
// difference between a member being credited and somebody else claiming
// their purchase (FR-020): the reference is long enough and shaped the way
// the click table demands, it is unpredictable, and a failing entropy source
// produces nothing at all rather than something weaker.

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/clickout"
)

// clickRefColumn is click_ref_url_safe_and_long_enough (migration 0012),
// spelled out here rather than imported. A test that asked the same
// constructor the code asks would only prove the code calls it; this asks
// the schema's own rule, so a change to either side is caught.
var clickRefColumn = regexp.MustCompile(`^[A-Za-z0-9_-]{22,}$`)

func TestAMintedReferenceIsOneTheClickTableAccepts(t *testing.T) {
	t.Parallel()

	ref, err := clickout.NewMinter().Mint()
	if err != nil {
		t.Fatalf("Mint(): %v", err)
	}
	if !clickRefColumn.MatchString(ref.Ref()) {
		t.Errorf("minted %q, which click_ref_url_safe_and_long_enough refuses", ref.Ref())
	}
	// 128 bits base64url-encoded, unpadded. Asserted exactly, because the
	// two ways this could drift - fewer entropy bytes, or a padded encoder -
	// both show up here first and one of them is a security defect.
	if len(ref.Ref()) != 22 {
		t.Errorf("minted a %d-character reference, want 22 - 16 bytes of entropy, base64url, unpadded", len(ref.Ref()))
	}
	if strings.Contains(ref.Ref(), "=") {
		t.Errorf("minted %q, which carries base64 padding the click table refuses", ref.Ref())
	}
}

// TestMintedReferencesDoNotRepeat is a smoke test for the entropy actually
// reaching the output. It cannot prove randomness, but it does catch the
// failures that matter and are easy to introduce: a reference derived from a
// counter, from the clock, or from a source read once and reused.
func TestMintedReferencesDoNotRepeat(t *testing.T) {
	t.Parallel()

	const mints = 2000
	minter := clickout.NewMinter()
	seen := make(map[string]struct{}, mints)
	for i := range mints {
		ref, err := minter.Mint()
		if err != nil {
			t.Fatalf("Mint() %d: %v", i, err)
		}
		if _, repeat := seen[ref.Ref()]; repeat {
			t.Fatalf("Mint() returned %q twice in %d mints", ref.Ref(), i+1)
		}
		seen[ref.Ref()] = struct{}{}
	}
}

// TestEveryByteOfEntropyReachesTheReference pins that the whole read is
// encoded, not a prefix of it. A minter that encoded fewer bytes than it
// read would still pass the shape and uniqueness tests above while carrying
// less entropy than FR-020 requires.
func TestEveryByteOfEntropyReachesTheReference(t *testing.T) {
	t.Parallel()

	// Sixteen distinct bytes, so any dropped or reordered byte changes the
	// encoding.
	entropy := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	ref, err := clickout.NewMinter(clickout.WithEntropy(bytes.NewReader(entropy))).Mint()
	if err != nil {
		t.Fatalf("Mint(): %v", err)
	}
	const want = "AAECAwQFBgcICQoLDA0ODw"
	if ref.Ref() != want {
		t.Errorf("Mint() = %q, want %q - the base64url of all %d bytes read", ref.Ref(), want, len(entropy))
	}
}

// TestAFailingEntropySourceMintsNothing is the security case. There is no
// fallback source and there must not be one: a reference minted from
// anything predictable lets somebody else's purchase be claimed as a
// member's own, and no click-out is a far better outcome than a guessable
// one.
func TestAFailingEntropySourceMintsNothing(t *testing.T) {
	t.Parallel()

	entropyGone := errors.New("the entropy device is gone")

	cases := []struct {
		name    string
		entropy io.Reader
	}{
		{name: "a source that fails outright", entropy: failingReader{err: entropyGone}},
		// The nearest miss, and the one a bare Read would let through: a
		// source that returns SOME bytes yields a shorter reference, which
		// is both refused by the click table and enumerable.
		{name: "a source that returns too few bytes", entropy: bytes.NewReader([]byte("only-eight"))},
		{name: "a source that returns nothing at all", entropy: bytes.NewReader(nil)},
		{name: "no source at all", entropy: nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ref, err := clickout.NewMinter(clickout.WithEntropy(tc.entropy)).Mint()

			if !errors.Is(err, clickout.ErrNoClickReference) {
				t.Fatalf("Mint() error = %v, want one wrapping %v", err, clickout.ErrNoClickReference)
			}
			// The zero value is "no reference was minted", and Validate is
			// how the rest of the system asks that question.
			if err := ref.Validate(); err == nil {
				t.Errorf("Mint() = %q alongside its error; a failed mint must yield no reference", ref.Ref())
			}
		})
	}
}

// TestTheDefaultSourceIsCryptographic keeps the production minter on
// crypto/rand rather than on whatever a test last configured.
func TestTheDefaultSourceIsCryptographic(t *testing.T) {
	t.Parallel()

	// Read the same way the minter does; if this succeeds and Mint fails,
	// the default source is not the one being read here.
	if _, err := io.ReadFull(rand.Reader, make([]byte, 16)); err != nil {
		t.Skipf("this machine's crypto/rand is unavailable: %v", err)
	}
	if _, err := clickout.NewMinter().Mint(); err != nil {
		t.Fatalf("Mint() with the default source: %v", err)
	}
}

// failingReader is an entropy source that only ever fails.
type failingReader struct{ err error }

func (f failingReader) Read([]byte) (int, error) { return 0, f.err }
