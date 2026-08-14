// Package translation owns machine translation: an LLM adapter behind an
// interface (swappable providers, per-article cost ceiling, monthly cap) and
// lineage recording - every translation row carries model, prompt version
// and generation time (I-5).
//
// Boundary: no other module imports this package's internals. Consumers
// define the interfaces they need and the composition root in cmd wires them.
package translation
