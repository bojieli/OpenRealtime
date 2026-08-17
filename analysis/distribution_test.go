package analysis

import "testing"

func TestSummarizeNearestRank(t *testing.T) {
	t.Parallel()
	distribution, err := Summarize([]uint64{5, 1, 4, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	if distribution.Count != 5 || distribution.MinNS != 1 || distribution.P50NS != 3 || distribution.P90NS != 5 || distribution.MaxNS != 5 {
		t.Fatalf("unexpected distribution: %+v", distribution)
	}
	if _, err := Summarize(nil); err == nil {
		t.Fatal("empty distribution must fail")
	}
	signed, err := SummarizeSigned([]int64{10, -20, 0})
	if err != nil {
		t.Fatal(err)
	}
	if signed.MinNS != -20 || signed.P50NS != 0 || signed.MaxNS != 10 {
		t.Fatalf("unexpected signed distribution: %+v", signed)
	}
}
