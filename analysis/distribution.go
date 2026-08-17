// Package analysis contains deterministic, dependency-free experiment summaries.
package analysis

import (
	"errors"
	"slices"
)

type Distribution struct {
	Count uint64 `json:"count"`
	MinNS uint64 `json:"min_ns"`
	P50NS uint64 `json:"p50_ns"`
	P90NS uint64 `json:"p90_ns"`
	P95NS uint64 `json:"p95_ns"`
	P99NS uint64 `json:"p99_ns"`
	MaxNS uint64 `json:"max_ns"`
}

type SignedDistribution struct {
	Count uint64 `json:"count"`
	MinNS int64  `json:"min_ns"`
	P50NS int64  `json:"p50_ns"`
	P90NS int64  `json:"p90_ns"`
	P95NS int64  `json:"p95_ns"`
	P99NS int64  `json:"p99_ns"`
	MaxNS int64  `json:"max_ns"`
}

type ScalarDistribution struct {
	Count uint64 `json:"count"`
	Min   uint64 `json:"min"`
	P50   uint64 `json:"p50"`
	P90   uint64 `json:"p90"`
	P95   uint64 `json:"p95"`
	P99   uint64 `json:"p99"`
	Max   uint64 `json:"max"`
}

func Summarize(values []uint64) (Distribution, error) {
	if len(values) == 0 {
		return Distribution{}, errors.New("distribution requires at least one value")
	}
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	return Distribution{
		Count: uint64(len(sorted)), MinNS: sorted[0], P50NS: percentile(sorted, 50),
		P90NS: percentile(sorted, 90), P95NS: percentile(sorted, 95),
		P99NS: percentile(sorted, 99), MaxNS: sorted[len(sorted)-1],
	}, nil
}

func SummarizeScalar(values []uint64) (ScalarDistribution, error) {
	if len(values) == 0 {
		return ScalarDistribution{}, errors.New("scalar distribution requires at least one value")
	}
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	return ScalarDistribution{
		Count: uint64(len(sorted)), Min: sorted[0], P50: percentile(sorted, 50),
		P90: percentile(sorted, 90), P95: percentile(sorted, 95),
		P99: percentile(sorted, 99), Max: sorted[len(sorted)-1],
	}, nil
}

func SummarizeSigned(values []int64) (SignedDistribution, error) {
	if len(values) == 0 {
		return SignedDistribution{}, errors.New("signed distribution requires at least one value")
	}
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	return SignedDistribution{
		Count: uint64(len(sorted)), MinNS: sorted[0], P50NS: signedPercentile(sorted, 50),
		P90NS: signedPercentile(sorted, 90), P95NS: signedPercentile(sorted, 95),
		P99NS: signedPercentile(sorted, 99), MaxNS: sorted[len(sorted)-1],
	}, nil
}

func percentile(sorted []uint64, percent uint64) uint64 {
	index := (percent*uint64(len(sorted)) + 99) / 100
	if index == 0 {
		return sorted[0]
	}
	return sorted[index-1]
}

func signedPercentile(sorted []int64, percent uint64) int64 {
	index := (percent*uint64(len(sorted)) + 99) / 100
	if index == 0 {
		return sorted[0]
	}
	return sorted[index-1]
}
