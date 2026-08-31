// Package earnings turns what a network reported into what a member is owed.
//
// It sits between ingestion, which records what a network said without
// deciding anything, and the ledger, which moves money. Three questions in
// order: which click earned this purchase (T067), what the member's share of
// it is (T068), and what state the resulting entry is in (T069). They are
// separate on purpose - a mis-resolved reference should produce a queue row
// an operator can see, never a credit nobody authorised.
//
// Two rules shape the package. The click-time snapshot governs, not the offer
// as it stands when the money finally arrives (FR-013): a member who clicked
// a published rate is owed that rate even if it has since been withdrawn.
// And a report that cannot be resolved is RECORDED rather than dropped
// (FR-034) - the money is real whether or not Apivo can say whose it is, and
// a transaction in no queue at all is one nobody will ever look for.
package earnings
