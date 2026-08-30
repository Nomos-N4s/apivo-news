// Retrieval: what Apivo knows about one read that the network does not, and
// why the port refuses to carry it. One file, because it is a value the
// write path takes and the port deliberately does not have.

package networks

import (
	"errors"
	"fmt"
	"time"
)

var (
	// ErrInvalidRetrieval reports a retrieval that describes no read: no
	// publisher account, no moment, or a window a network could not have
	// been asked for.
	//
	// It is separate from the sentinels that refuse a [Reported] because the
	// two send an investigation to opposite places. A bad report is the
	// adapter's mistake, in code that translates one network's answer; a bad
	// retrieval is the POLLER's, in code that decides what to ask and when -
	// and the row that would be written from it is evidence, which cannot be
	// corrected afterwards (C-3). Both refused before the insert, both named
	// so an operator reads which.
	ErrInvalidRetrieval = errors.New("networks: retrieval describes no read of a network")
)

// Retrieval is the half of an evidence row that the network did not supply:
// which publisher account made the read, at what moment it was made, and for
// which query window.
//
// The [Network] port carries none of it, and that is a decision rather than
// an omission (contracts/ports.md section 2). An adapter translates and
// nothing else: the moment of a read and the period it covered are Apivo's
// own account of its own behaviour, and an adapter that reported them would
// be reporting on the poller rather than on the network. Every one of these
// is a NOT NULL column on cashback.network_transaction, so the poller
// supplies them here, where the row is actually written.
//
// The one exception the port does carry is the publisher account
// ([Network.Account]), because the poller cannot record a column it has no
// way to learn - the account is the adapter's identity, and the same network
// may be polled through two of them.
//
// It is a plain struct rather than a constructor-guarded value. Unlike a
// click reference, nothing here is minted: these are three facts the poller
// already holds, and a constructor would only move the same check earlier
// while making a caller assemble the value twice. [Retrieval.Validate] is
// called by the writer before anything is inserted, which is the point that
// matters - the row it guards is immutable.
type Retrieval struct {
	// Account is the publisher account the read was made through, and what
	// every evidence row carries in network_account_id. It owns both durable
	// cursors, so a row filed under the wrong one resumes another account's
	// window from this one's watermark.
	Account PublisherAccount
	// RetrievedAt is when the network was asked, as Apivo observed it. It is
	// stated rather than left to the server's default so that every row of
	// one window carries ONE instant: a default would stamp each row with
	// the moment it happened to be inserted, and a window that took a minute
	// to persist would look like a minute of separate retrievals.
	RetrievedAt time.Time
	// Window is the period that was asked for. It is stored beside the
	// report because a re-read of the same period is how a pending
	// transaction is ever seen to become confirmed (contract rule 4), and an
	// operator asking why a transaction was missed needs to know which
	// windows were actually requested rather than which ones should have
	// been.
	Window QueryWindow
}

// Validate refuses a retrieval that describes no read, wrapping
// [ErrInvalidRetrieval] and naming the part at fault.
//
// The window is held to [QueryWindow.Validate] and nothing more. Whether it
// was wider than a network allows is [Limits.ValidateWindow]'s question and
// was answered before the read; asking it again here would need limits this
// value does not carry, and a window that has already been answered is a
// window that was allowed.
func (r Retrieval) Validate() error {
	if err := r.Account.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidRetrieval, err)
	}
	if r.RetrievedAt.IsZero() {
		return fmt.Errorf("%w: %s names no moment it was retrieved at", ErrInvalidRetrieval, r.Account)
	}
	if err := r.Window.Validate(); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrInvalidRetrieval, r.Account, err)
	}
	return nil
}

// String names the read for a log or a refusal: the account, the window, and
// the moment. It is what an operator needs to find the poll that wrote a
// row, so all three appear and none is abbreviated.
func (r Retrieval) String() string {
	return fmt.Sprintf("%s over %s retrieved at %s",
		r.Account, r.Window, r.RetrievedAt.UTC().Format(time.RFC3339Nano))
}
