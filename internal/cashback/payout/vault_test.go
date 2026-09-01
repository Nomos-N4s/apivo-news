package payout_test

// The stand-in vault the handler cases are built with.

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/payout"
)

// stubVault hands back a reference and remembers nothing about what it was
// given - which is what a vault looks like from this side of the port.
type stubVault struct {
	// refuse makes the next store refuse the details as a member's mistake.
	refuse bool
	// broken makes it fail as a vault that is down rather than a member who
	// is wrong; the two get different answers.
	broken error
	// seen is the kinds it was asked about. Deliberately NOT the details:
	// a fake that kept them would be the leak the port exists to prevent,
	// and a test asserting on them would entrench it.
	seen []payout.Kind
}

func (v *stubVault) Store(_ context.Context, kind payout.Kind, details json.RawMessage) (string, error) {
	v.seen = append(v.seen, kind)
	switch {
	case v.broken != nil:
		return "", v.broken
	case v.refuse:
		return "", payout.ErrDetailsRefused
	case len(details) == 0:
		return "", payout.ErrDetailsRefused
	}
	return "vault:" + kind.String() + ":" + uuid.NewString(), nil
}

// errAVaultDown stands in for a vault that is not working, as opposed to a
// member whose details are wrong. The two get different answers.
var errAVaultDown = errors.New("the vault is not answering")
