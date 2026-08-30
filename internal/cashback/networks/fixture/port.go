// [New], which builds the adapter, and the one line that says a *[Network] is
// a [networks.Network]. One file, and necessarily the last one written:
// both statements mention the whole port at once - New because it hands the
// finished adapter to [networks.ValidateNetwork], the assertion because that
// is its entire job - so neither could compile until every method the port
// names existed. Keeping them together makes that ordering obvious rather
// than mysterious.

package fixture

import (
	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
)

// The port is satisfied by *Network or not at all; this line is where the
// compiler says so, and it is the difference between an adapter and a
// package that merely has similar-looking methods.
var _ networks.Network = (*Network)(nil)

// New builds a fixture adapter for one publisher account, applying opts in
// order and refusing anything the composition root would refuse.
//
// The account is an argument rather than an option because an adapter without
// one polls on behalf of nobody: every evidence row requires
// network_account_id, both durable cursors live on the account, and the
// poller keys its registry by it (contract rule 10). The account must be held
// at this adapter's own network - [ID] - which is what
// [networks.ValidateNetwork] refuses below, before an adapter can file one
// network's transactions under another's id.
//
// It refuses rather than returning something half-built, and it does so at
// wiring, which is where all three of its failures are cheap. A recording
// this package cannot read is a defect in this repository
// ([ErrRecordingUnreadable]); an account that names no row or is held at
// another network is a wiring mistake ([networks.ErrInvalidPublisherAccount]);
// limits that describe no queryable network are a forgotten declaration
// ([networks.ErrInvalidLimits]) that would otherwise make every window too
// wide and stop ingestion dead with nobody told why.
func New(account networks.PublisherAccount, opts ...Option) (*Network, error) {
	adapter, err := newAdapter(account, opts...)
	if err != nil {
		return nil, err
	}
	if err := networks.ValidateNetwork(adapter); err != nil {
		return nil, err
	}
	return adapter, nil
}
