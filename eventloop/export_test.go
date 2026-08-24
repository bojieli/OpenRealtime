package eventloop

// SelectRunnableForTest exposes the partition so the rule can be asserted
// directly. It is the one place that decides whether a branch raised because
// work is in flight gets to run while that work is still in flight.
func SelectRunnableForTest(deferred []Batch, active bool) (Batch, []Batch) {
	return selectRunnable(deferred, active)
}
