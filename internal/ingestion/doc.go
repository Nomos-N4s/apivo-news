// Package ingestion owns feed polling, item normalisation and provenance
// capture. It is the only writer of source and source_item rows, and it must
// write provenance in the same transaction as the content (I-2) with the
// source's licence terms snapshotted at retrieval (I-4).
//
// Boundary: no other module imports this package's internals. Consumers
// define the interfaces they need and the composition root in cmd wires them.
package ingestion
