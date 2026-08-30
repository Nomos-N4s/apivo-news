// The tests for identity.go: what makes a [networks.NetworkID] the word the
// network table is keyed by. It also holds portTestAssert, the judgement
// every table in this package's tests is read through, because this is the
// lowest-dependency file that needs it.

package networks_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
)

// portTestAssert is the shared judgement every table below uses: the call
// either succeeds or fails wrapping exactly the named sentinel, and a
// refusal must point at the offending value rather than merely naming a
// rule.
func portTestAssert(t *testing.T, what string, err error, wantErr error, wantIn []string) {
	t.Helper()
	if wantErr == nil {
		if err != nil {
			t.Fatalf("%s = %v, want nil", what, err)
		}
		return
	}
	if err == nil {
		t.Fatalf("%s = nil, want an error wrapping %v", what, wantErr)
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("%s = %v, want an error wrapping %v", what, err, wantErr)
	}
	for _, fragment := range wantIn {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("%s = %q, want it to mention %q", what, err, fragment)
		}
	}
}

func TestNetworkIDValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		id      networks.NetworkID
		wantErr error
	}{
		{name: "awin", id: "awin"},
		{name: "an id with digits and underscores", id: "tradedoubler_2"},
		{name: "the empty id a bare literal carries", id: "", wantErr: networks.ErrInvalidNetworkID},
		{name: "an uppercase id", id: "Awin", wantErr: networks.ErrInvalidNetworkID},
		{name: "an id with an uppercase letter after the first", id: "traDedoubler", wantErr: networks.ErrInvalidNetworkID},
		{name: "an id starting with a digit", id: "2awin", wantErr: networks.ErrInvalidNetworkID},
		{name: "an id with a hyphen", id: "trade-doubler", wantErr: networks.ErrInvalidNetworkID},
		{name: "a padded id", id: " awin", wantErr: networks.ErrInvalidNetworkID},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			portTestAssert(t, "NetworkID.Validate()", tc.id.Validate(), tc.wantErr, nil)
		})
	}
}
