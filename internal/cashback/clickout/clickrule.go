// What the click rule IS: two numbers and one window (T066, US7 scenario 1).
//
// A file of its own because the rule is the part an operator reasons about,
// and it should be readable without the machinery that applies it.

package clickout

import "time"

// The rule a deployment gets when it configures none. Generous by design:
// this is a guardrail against a flood, not a quota, and a member browsing a
// catalogue hard on a payday should never meet it.
//
// The window is a package constant rather than a knob. Two numbers and one
// window is a rule somebody can hold in their head; three numbers is one
// they have to reason about, and the reasoning would happen while looking at
// a graph of members who could not click.
const (
	// ClickWindow is how far back the rule counts.
	ClickWindow = time.Hour
	// DefaultClicksPerMember is how many click-outs one member may make in
	// that window.
	DefaultClicksPerMember = 60
	// DefaultClicksPerContext is how many one DEVICE may make, where a
	// deployment can tell devices apart. Higher than the per-member figure
	// on purpose: a household or an office is several members behind one
	// address, and this half exists to catch a flood rather than to bound a
	// family.
	DefaultClicksPerContext = 120
)

// ClickRule is how many click-outs are allowed in [ClickWindow].
type ClickRule struct {
	// PerMember bounds one account. Always applied: the account id is the
	// caller's own identity, so this half is always keyed on the right
	// thing.
	PerMember int
	// PerContext bounds one device or context digest. Zero turns this half
	// OFF, and zero is the right default for a deployment that cannot tell
	// devices apart.
	//
	// That is not caution for its own sake. A digest built from the address
	// the API sees is the PROXY's address when the API sits behind one, so
	// every member shares one context - and a per-context limit would then
	// throttle the whole site the moment any one of them clicked briskly.
	// The composition root turns this half on only where the deployment
	// names a header it trusts to carry the real client address.
	PerContext int
}

// DefaultClickRule is the rule with the per-member half at its default and
// the per-context half off - which is right wherever the deployment cannot
// tell devices apart.
func DefaultClickRule() ClickRule {
	return ClickRule{PerMember: DefaultClicksPerMember}
}

// DefaultClickRuleWithContext is the rule for a deployment that names a
// header it trusts to carry the real client address: both halves on.
func DefaultClickRuleWithContext() ClickRule {
	return ClickRule{PerMember: DefaultClicksPerMember, PerContext: DefaultClicksPerContext}
}

// Applies reports whether this rule bounds anything at all.
func (r ClickRule) Applies() bool { return r.PerMember > 0 || r.PerContext > 0 }
