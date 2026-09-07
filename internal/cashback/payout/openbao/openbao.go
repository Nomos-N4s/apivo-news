// Package openbao implements the [payout.DetailsVault] port over OpenBao's
// KV version 2 secrets engine (ADR-0006).
//
// It is the answer to a question the port left open on purpose: bank details
// have to live somewhere this database is not, and which somewhere was a
// deployment's decision until it was made. OpenBao is that decision - the
// MPL-2.0 fork of Vault, self-hosted, with the details held as secrets
// rather than as ciphertext beside the rows that reference them.
//
// Nothing here escapes this directory. The package speaks the port's types
// and OpenBao's HTTP API, and the composition root wires it from
// configuration strings without the rest of the product learning the
// vendor's name - the same shape ADR-0002 fixed for the ledger and ADR-0003
// for network adapters.
//
// # One-way, like the port
//
// There is no read method here and there must not be one. The port can put
// details in and get a reference back, because a rail resolves the reference
// itself and nothing in this product ever needs the details again. Today the
// rail is a person, and they read the IBAN in OpenBao's own interface - which
// is outside this codebase by design, and is what keeps a read path from
// existing here for somebody to eventually use.
//
// # What the reference is
//
// A random path, and nothing else. The port requires a reference that does
// not embed the details, and a path derived from them - a hash of the IBAN,
// say - would be a reference that reveals whether two members bank at the
// same account. The identifier is 128 bits from crypto/rand, so it says
// nothing about what it points at.
package openbao

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/payout"
)

// This package implements the port and is held to it by the compiler, so a
// signature that drifted fails the build here rather than in the root.
var _ payout.DetailsVault = (*Vault)(nil)

const (
	// DefaultMount is where OpenBao mounts a KV v2 engine unless told
	// otherwise, and matches `bao secrets enable -path=secret kv-v2`.
	DefaultMount = "secret"
	// DefaultPrefix keeps this product's secrets in one subtree, so a
	// policy can be written against them and an operator can find them.
	DefaultPrefix = "cashback/payout-destinations"
	// DefaultTimeout bounds one call. Storing details happens inside a
	// member's request, so a vault that has stopped answering must fail
	// that request rather than hold the connection until something else
	// gives up.
	DefaultTimeout = 5 * time.Second
	// maxDetails bounds what will be forwarded. A rail needs an account
	// number and a name; anything at this size is a mistake or an attempt
	// to use the vault as storage, and neither should reach it.
	maxDetails = 8 << 10
)

// Vault stores payout details in OpenBao and answers the reference the
// database records.
type Vault struct {
	endpoint string
	token    string
	mount    string
	prefix   string
	timeout  time.Duration
	client   *http.Client
	newID    func() (string, error)
}

// Option configures a [Vault].
type Option func(*Vault)

// WithToken authenticates to OpenBao. Without one every call is refused by
// the server, which is the correct behaviour for a misconfigured deployment
// - it fails loudly on the first destination rather than storing secrets
// somewhere unauthenticated.
func WithToken(token string) Option {
	return func(v *Vault) { v.token = strings.TrimSpace(token) }
}

// WithMount names the KV v2 mount, when it is not [DefaultMount].
func WithMount(mount string) Option {
	return func(v *Vault) { v.mount = strings.Trim(strings.TrimSpace(mount), "/") }
}

// New builds the vault against an OpenBao endpoint.
//
// The endpoint is never repeated in an error. It travels beside a token in
// configuration, and a token pasted into the wrong key would otherwise print
// itself into a startup log somebody keeps.
func New(endpoint string, opts ...Option) (*Vault, error) {
	v := &Vault{
		mount:   DefaultMount,
		prefix:  DefaultPrefix,
		timeout: DefaultTimeout,
		newID:   randomID,
	}
	for _, opt := range opts {
		opt(v)
	}
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, errors.New("openbao: the vault endpoint must be an absolute http or https URL (the value is not repeated here: it may carry a credential)")
	}
	if v.mount == "" || v.prefix == "" {
		return nil, errors.New("openbao: the mount and the prefix must both name somewhere")
	}
	if v.client == nil {
		v.client = &http.Client{}
	}
	v.endpoint = strings.TrimRight(parsed.String(), "/")
	return v, nil
}

// Store writes one destination's details and answers its reference.
//
// The details are checked against the kind first, because the port says this
// is the only thing positioned to: what counts as valid differs by rail, and
// an IBAN that fails its checksum is a payment that bounces days later
// rather than a request refused now. A refusal wraps
// [payout.ErrDetailsRefused] and is the member's to act on; everything else
// is this deployment's.
//
// Nothing that is refused is quoted back. An error is the least controlled
// string in a system, and one carrying an IBAN reaches a log, a trace and a
// support ticket.
func (v *Vault) Store(ctx context.Context, kind payout.Kind, details json.RawMessage) (string, error) {
	if len(details) > maxDetails {
		return "", fmt.Errorf("%w: a %s destination's details are larger than this vault will carry", payout.ErrDetailsRefused, kind)
	}
	if err := validate(kind, details); err != nil {
		return "", err
	}

	id, err := v.newID()
	if err != nil {
		return "", fmt.Errorf("openbao: minting a reference: %w", err)
	}
	path := v.prefix + "/" + id

	// The kind is stored beside the details rather than left implicit. The
	// person who eventually opens this secret is making a bank transfer,
	// and which rail it is for is the first thing they need.
	body, err := json.Marshal(struct {
		Data map[string]json.RawMessage `json:"data"`
	}{Data: map[string]json.RawMessage{
		"kind":    json.RawMessage(jsonString(kind.String())),
		"details": details,
	}})
	if err != nil {
		return "", fmt.Errorf("openbao: building the request: %w", err)
	}

	call, cancel := context.WithTimeout(ctx, v.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(call, http.MethodPost,
		v.endpoint+"/v1/"+v.mount+"/data/"+path, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("openbao: building the request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if v.token != "" {
		req.Header.Set("X-Vault-Token", v.token)
	}

	res, err := v.client.Do(req)
	if err != nil {
		// The URL is deliberately not in this error: http's own error
		// already carries it, and this wrapper must not add a second copy
		// beside a token.
		return "", fmt.Errorf("openbao: storing payout details: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
	}()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		// The response body is NOT included. OpenBao echoes the request
		// path in its errors, and a body forwarded from here would put the
		// secret's location into a log beside the reason it failed.
		return "", fmt.Errorf("openbao: storing payout details: the vault answered %s", res.Status)
	}
	return Reference(v.mount, path), nil
}

// Reference renders what cashback.payout_destination.details_ref holds.
//
// Scheme-prefixed, like every other reference in this schema
// (`config:networks.awin.credential`), so a row says which store it points
// into. A later migration to another vault can tell what it is looking at
// without a column recording it separately.
func Reference(mount, path string) string { return "openbao:" + mount + "/" + path }

// validate checks the details against the rail that would carry them.
//
// Only sepa has a shape worth checking, and it is worth checking hard: an
// IBAN is the one field here whose correctness a standard can settle, and a
// wrong one is a payment that fails or reaches somebody else. The other
// kinds are deliberately open, as the contract says - "what belongs in it
// differs by rail" - so all that is asked of them is that they say
// something.
func validate(kind payout.Kind, details json.RawMessage) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(details, &fields); err != nil {
		return fmt.Errorf("%w: a %s destination's details must be an object", payout.ErrDetailsRefused, kind)
	}
	if len(fields) == 0 {
		return fmt.Errorf("%w: a %s destination's details say nothing", payout.ErrDetailsRefused, kind)
	}
	if kind != payout.KindSEPA {
		return nil
	}

	var iban, holder string
	if raw, ok := fields["iban"]; ok {
		_ = json.Unmarshal(raw, &iban)
	}
	if raw, ok := fields["holder"]; ok {
		_ = json.Unmarshal(raw, &holder)
	}
	if strings.TrimSpace(holder) == "" {
		return fmt.Errorf("%w: a sepa destination names the account holder", payout.ErrDetailsRefused)
	}
	if !ValidIBAN(iban) {
		return fmt.Errorf("%w: that is not an IBAN", payout.ErrDetailsRefused)
	}
	return nil
}

// ValidIBAN reports whether s is a structurally valid IBAN that passes its
// own check digits (ISO 13616).
//
// The checksum is the half that earns its place. Structure alone accepts a
// transposed pair of digits, which is the commonest way an account number is
// mistyped, and the mod-97 check refuses it - which is the whole point of
// the two check digits being there.
//
// It says nothing about whether the account exists. Nothing short of the
// banking system can, and a vault that implied otherwise would be worse than
// one that checked nothing.
func ValidIBAN(s string) bool {
	compact := strings.ToUpper(strings.NewReplacer(" ", "", "\t", "").Replace(strings.TrimSpace(s)))
	if len(compact) < 15 || len(compact) > 34 {
		return false
	}
	for i, c := range compact {
		switch {
		case i < 2 && (c < 'A' || c > 'Z'):
			return false
		case i >= 2 && i < 4 && (c < '0' || c > '9'):
			return false
		case i >= 4 && (c < '0' || c > '9') && (c < 'A' || c > 'Z'):
			return false
		}
	}
	// Rearranged, then reduced a few digits at a time: the number is far
	// wider than 64 bits, and a running remainder is the standard way to
	// take mod 97 of it without a big-integer type.
	rearranged := compact[4:] + compact[:4]
	remainder := 0
	for _, c := range rearranged {
		switch {
		case c >= '0' && c <= '9':
			remainder = remainder*10 + int(c-'0')
		default:
			remainder = remainder*100 + int(c-'A') + 10
		}
		remainder %= 97
	}
	return remainder == 1
}

// randomID mints the path segment a reference points at: 128 bits from
// crypto/rand, hex-encoded, derived from nothing.
func randomID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

// jsonString renders a Go string as a JSON string, so the kind can be
// placed into the payload beside the caller's already-encoded details
// without decoding those first.
func jsonString(s string) string {
	encoded, err := json.Marshal(s)
	if err != nil {
		// Unreachable: a Go string always marshals.
		return `""`
	}
	return string(encoded)
}
