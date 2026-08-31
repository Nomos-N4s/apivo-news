// The tests for fixture.go: what the adapter says about itself, and what each
// knob does to it. They build the adapter through newAdapter rather than
// through [New], because New's extra job - holding the finished adapter to
// [networks.ValidateNetwork] - is port.go's to test.

package fixture

import (
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
)

// fixtureTestAccount is a publisher account at this adapter's own network:
// the one shape [networks.ValidateNetwork] accepts, so that a test failing
// for any other reason is not failing over its wiring.
func fixtureTestAccount(t *testing.T) networks.PublisherAccount {
	t.Helper()
	account, err := networks.NewPublisherAccount(uuid.New(), ID, "fixture-publisher-1")
	if err != nil {
		t.Fatalf("building the publisher account: %v", err)
	}
	return account
}

// fixtureTestAdapter builds an adapter, failing the test rather than
// returning an error, because every caller of it is testing something else.
func fixtureTestAdapter(t *testing.T, opts ...Option) *Network {
	t.Helper()
	adapter, err := newAdapter(fixtureTestAccount(t), opts...)
	if err != nil {
		t.Fatalf("newAdapter(): %v", err)
	}
	return adapter
}

func TestAdapterStatesWhoItIs(t *testing.T) {
	t.Parallel()

	account := fixtureTestAccount(t)
	adapter, err := newAdapter(account)
	if err != nil {
		t.Fatalf("newAdapter(): %v", err)
	}
	if got := adapter.ID(); got != ID {
		t.Errorf("ID() = %q, want %q", got, ID)
	}
	if got := adapter.Account(); got != account {
		t.Errorf("Account() = %s, want %s", got, account)
	}
	if got := adapter.Stage(); got != StageClick {
		t.Errorf("a freshly built adapter is at %s, want %s: nobody has polled it yet", got, StageClick)
	}
}

// TestAdapterDefaultsToTheReferenceLimits pins the numbers backfill is
// windowed by. They are the reference network's documented pair (ADR-0003),
// and a fixture declaring anything else would have every caller written
// against a window width no real network allows.
func TestAdapterDefaultsToTheReferenceLimits(t *testing.T) {
	t.Parallel()

	limits := fixtureTestAdapter(t).Limits()
	if err := limits.Validate(); err != nil {
		t.Fatalf("the default limits describe no queryable network: %v", err)
	}
	if want := 31 * 24 * time.Hour; limits.MaxWindow != want {
		t.Errorf("MaxWindow = %s, want %s", limits.MaxWindow, want)
	}
	if want := 360; limits.RequestsPerMinute != want {
		t.Errorf("RequestsPerMinute = %d, want %d", limits.RequestsPerMinute, want)
	}
}

func TestWithLimitsReplacesTheDeclaration(t *testing.T) {
	t.Parallel()

	want := networks.Limits{MaxWindow: 2 * time.Hour, RequestsPerMinute: 1}
	if got := fixtureTestAdapter(t, WithLimits(want)).Limits(); got != want {
		t.Errorf("Limits() = %+v, want %+v", got, want)
	}
}

func TestWithStageStartsPartwayThroughTheLifecycle(t *testing.T) {
	t.Parallel()

	for stage := StageClick; stage <= StageReversed; stage++ {
		if got := fixtureTestAdapter(t, WithStage(stage)).Stage(); got != stage {
			t.Errorf("WithStage(%s) built an adapter at %s", stage, got)
		}
	}
}

// TestWithStageRefusesAStageTheRecordingHasNot keeps a caller's arithmetic
// out of the recording's index. A Stage is an int, and an adapter started
// outside the four would panic on its first read - in the one package every
// other package is tested against.
func TestWithStageRefusesAStageTheRecordingHasNot(t *testing.T) {
	t.Parallel()

	for _, stage := range []Stage{Stage(-1), Stage(stageCount), Stage(99)} {
		if _, err := newAdapter(fixtureTestAccount(t), WithStage(stage)); !errors.Is(err, ErrOptionRefused) {
			t.Errorf("newAdapter(WithStage(%d)) error = %v, want one wrapping ErrOptionRefused", int(stage), err)
		}
	}
}

func TestWithUnmappableStatusIsOffUnlessAskedFor(t *testing.T) {
	t.Parallel()

	if fixtureTestAdapter(t).unmappable {
		t.Error("a fixture nobody configured serves the unmappable page; rule 2's proof would then fire in every unrelated test")
	}
	if !fixtureTestAdapter(t, WithUnmappableStatus()).unmappable {
		t.Error("WithUnmappableStatus() did not turn the knob on")
	}
}

func TestWithFailureAndSetFailureAgree(t *testing.T) {
	t.Parallel()

	built := fixtureTestAdapter(t, WithFailure(FailureRateLimited, 3))
	if kind, left := built.failures.peek(); kind != FailureRateLimited || left != 3 {
		t.Errorf("WithFailure left (%s, %d), want (%s, 3)", kind, left, FailureRateLimited)
	}

	live := fixtureTestAdapter(t)
	live.SetFailure(FailureRefused, FailureAlways)
	if kind, left := live.failures.peek(); kind != FailureRefused || left != FailureAlways {
		t.Errorf("SetFailure left (%s, %d), want (%s, %d)", kind, left, FailureRefused, FailureAlways)
	}
	live.SetFailure(FailureNone, FailureAlways)
	if kind, _ := live.failures.peek(); kind != FailureNone {
		t.Errorf("SetFailure(FailureNone) left %s standing", kind)
	}
}

// TestDeeplinkTargetDescribesTheAwkwardRoute holds the shape of the route the
// adapter hands out. A caller left to invent a target would invent the one
// that happens to work - a bare path with no query of its own - and never
// exercise the case BuildDeeplink actually has to get right.
func TestDeeplinkTargetDescribesTheAwkwardRoute(t *testing.T) {
	t.Parallel()

	offerID := uuid.New()
	target := fixtureTestAdapter(t).DeeplinkTarget(offerID)

	if target.OfferID != offerID {
		t.Errorf("OfferID = %s, want %s", target.OfferID, offerID)
	}
	if target.NetworkID != ID {
		t.Errorf("NetworkID = %q, want %q", target.NetworkID, ID)
	}
	if target.ClickRefParam == "" {
		t.Error("the target names no click-reference parameter, so a redirect built from it would carry no attribution")
	}
	parsed, err := url.Parse(target.Template)
	if err != nil {
		t.Fatalf("the template is not a URL: %v", err)
	}
	if parsed.RawQuery == "" {
		t.Error("the template carries no query of its own; an adapter that only ever appended to a bare path works right up to the first operator who pastes in a campaign id")
	}
	if !strings.HasSuffix(parsed.Hostname(), ".invalid") {
		t.Errorf("the template points at %q; a fixture host that can resolve is eventually why a test suite sends real traffic somewhere", parsed.Hostname())
	}
}

// TestRecordedClickRefIsOneARedirectCouldHaveIssued keeps the constant and
// the click table in step. The recording echoes this reference back, and a
// demonstration mints it on the click - so a value the click table would
// refuse would make the whole attribution half of the chain unbuildable
// while every test here still passed.
func TestRecordedClickRefIsOneARedirectCouldHaveIssued(t *testing.T) {
	t.Parallel()

	if _, err := networks.NewIssuedClickRef(RecordedClickRef); err != nil {
		t.Fatalf("NewIssuedClickRef(RecordedClickRef): %v", err)
	}
}
