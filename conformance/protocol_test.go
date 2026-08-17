package conformance

import "testing"

func TestCompletePinnedProtocolRegistry(t *testing.T) {
	t.Parallel()
	report, err := RunProtocol()
	if err != nil {
		t.Fatal(err)
	}
	if !report.Passed || report.Definitions != 133 || report.UniqueWireTypes != 66 || len(report.Counts) != 8 ||
		!report.UnknownTypeRejected || !report.CrossDirectionRejected || !report.CrossProfileRejected || !report.MissingRequiredRejected {
		t.Fatalf("unexpected protocol conformance: %+v", report)
	}
}
