// [NetworkID]: the lowercase word the network table is keyed by, and the one
// format rule an id is held to. It is one file because naming a network is
// what every other declaration in this package does before it can say
// anything else, and the rule belongs beside the type it judges.

package networks

import (
	"errors"
	"fmt"
	"strconv"
)

// The sentinel a malformed network identity is refused with. Its rule is
// the format above; the package-wide convention is in doc.go.
var (
	// ErrInvalidNetworkID reports an identifier that is not the lowercase
	// word the network table is keyed by. The id is human-typed - it appears
	// in configuration and in operator conversation - so a mistyped one is
	// refused here rather than becoming a second network row beside the real
	// one, holding half its transactions. [ValidateNetwork] is where an
	// adapter's own id meets it, at wiring time.
	ErrInvalidNetworkID = errors.New("networks: network id is not a lowercase identifier")
)

// NetworkID identifies one affiliate network - "awin", "tradedoubler" - in
// the vocabulary the network table is keyed by (migration 0011). It is a
// stable, human-typed word rather than a surrogate id because it appears in
// configuration, in the fixture adapter's mode flag and in operator
// conversation, and a key nobody can say out loud is a key that gets
// mistyped into a second row.
type NetworkID string

// Valid reports whether id is a lowercase identifier: an ASCII lowercase
// letter followed by any number of lowercase letters, digits and
// underscores. It is the network_id_format check the schema already carries,
// applied here so that a bad id fails at the adapter rather than at the
// foreign key of the first transaction that referenced it. The empty id,
// which a bare struct literal carries, is not valid.
func (id NetworkID) Valid() bool {
	if id == "" {
		return false
	}
	if id[0] < 'a' || id[0] > 'z' {
		return false
	}
	for i := 1; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_':
		default:
			return false
		}
	}
	return true
}

// String returns the identifier itself, so a NetworkID prints as "awin"
// rather than as its underlying string type.
func (id NetworkID) String() string { return string(id) }

// Validate reports whether id names a network, returning an error wrapping
// [ErrInvalidNetworkID] when it does not.
func (id NetworkID) Validate() error {
	if !id.Valid() {
		return fmt.Errorf("%w: %s", ErrInvalidNetworkID, strconv.Quote(string(id)))
	}
	return nil
}
