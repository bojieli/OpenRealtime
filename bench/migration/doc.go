// Package migration compares a frozen baseline with a migration candidate.
//
// Unlike bench.Pair, this package is deliberately allowed to compare different
// source revisions, executables, configurations, or graph topologies when, and
// only when, those differences are named as treatment axes in a predeclared
// manifest. Every other declared provenance axis is held fixed.
//
// A comparison is an archival artifact rather than a convenient aggregate:
// the manifest enumerates the complete case/repetition population and an
// explicit per-case repetition floor, binds its margins to immutable
// baseline-variance evidence, and names the exact evidence kinds required for
// every attempt. Every input attempt is retained, exact pairs are visible, and
// invalid or incomplete input produces a sealed refusal instead of a result
// computed from a selected subset.
//
// Binary and latency confidence bounds use paired case-cluster bootstrap
// resampling, so repetitions of one case are not misrepresented as independent
// tasks. The conditional 80% candidate pass-rate floor is only an additional
// sanity rejection: it never substitutes for the paired non-inferiority,
// safety, or latency gates.
//
// Campaign reports record the exact history supplied to each invocation.
// VerifyWithHistory, MarshalWithHistory, WriteWithHistory, and
// ReferenceWithHistory prove that external lineage; the history-free variants
// intentionally accept only reports derived from an empty supplied history.
package migration
