// Package editorial owns the review queue, approval and publish flow. The
// approval gate is absolute: an article row only comes into existence when a
// named human approves (I-1); the database refuses anything else.
//
// Boundary: no other module imports this package's internals. Consumers
// define the interfaces they need and the composition root in cmd wires them.
package editorial
