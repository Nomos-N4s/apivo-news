// Package awin is the adapter for Awin, the affiliate network this
// deployment integrates (spec Q1, decided 2026-08-31; ADR-0003).
//
// Nothing outside this package knows Awin's vocabulary. That is the rule
// ADR-0003 draws the package boundary for, and the architecture test is
// what proves it rather than asserting it: a second network is a second
// package, and SC-008 is the claim that adding one changes only its own.
//
// The credentials never appear here. They arrive from the environment at
// construction and live only in the Authorization header of a request in
// flight; nothing in this package writes them to a log, an error, a URL or
// the database (ADR-0003).
package awin
