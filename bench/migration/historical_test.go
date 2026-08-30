package migration

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func testHistoricalBaselineRegistry(t *testing.T, manifest Manifest) HistoricalBaselineRegistry {
	t.Helper()
	baseline, _ := testAttempts()
	pass, interaction, deadline, safety := 0, 0, 0, 0
	latencies := make([]float64, 0, len(baseline))
	cases := map[string]bool{}
	for _, attempt := range baseline {
		cases[attempt.Key.Case] = true
		if attempt.Passed {
			pass++
		}
		if attempt.Outcomes.Interaction == OutcomeSatisfied {
			interaction++
		}
		if attempt.Outcomes.Deadline == OutcomeSatisfied {
			deadline++
		}
		if attempt.Outcomes.Safety == OutcomeFailed {
			safety++
		}
		latencies = append(latencies, attempt.Latencies[0].Value)
	}
	registry, err := SealHistoricalBaselineRegistry(HistoricalBaselineRegistry{
		Version:    HistoricalBaselineRegistryVersion,
		Authority:  "benchmark-owner",
		Acceptance: "The benchmark owner accepts the surviving original result numbers without historical per-attempt reconstruction.",
		ManifestID: manifest.ID(),
		Suites: []HistoricalSuiteBaseline{{
			Suite: "synthetic", ExpectedCases: len(cases), ExpectedAttempts: len(baseline),
			Trail: []string{"docs/measurement.md synthetic recorded baseline"},
			Pass: HistoricalRate{ApplicableAttempts: len(baseline),
				ApplicableCases: len(cases), Successes: pass},
			Interaction: &HistoricalRate{ApplicableAttempts: len(baseline),
				ApplicableCases: len(cases), Successes: interaction},
			Deadline: &HistoricalRate{ApplicableAttempts: len(baseline),
				ApplicableCases: len(cases), Successes: deadline},
			Safety: &HistoricalSafety{ApplicableAttempts: len(baseline),
				ApplicableCases: len(cases), Violations: safety},
			Latencies: []HistoricalLatency{{Name: "response_latency_ms", Unit: "ms",
				ApplicableCases: len(cases),
				Distribution:    distribution(latencies, "ms")}},
			Conditions: []HistoricalPartitionBaseline{},
			Cases:      []HistoricalPartitionBaseline{},
		}},
	}, manifest)
	if err != nil {
		t.Fatalf("seal historical baseline registry: %v", err)
	}
	return registry
}

func TestHistoricalBaselineRegistryRoundTripsCanonicalOwnerNumbers(t *testing.T) {
	manifest := testManifest()
	registry := testHistoricalBaselineRegistry(t, manifest)
	payload, err := MarshalHistoricalBaselineRegistry(registry, manifest)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeHistoricalBaselineRegistry(bytes.NewReader(payload), manifest)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.RegistryID != registry.RegistryID || decoded.ID() != registry.RegistryID ||
		decoded.Suites[0].Pass.Successes != 4 || decoded.Suites[0].Pass.ApplicableAttempts != 6 {
		t.Fatalf("decoded registry drifted: %+v", decoded)
	}

	reordered := registry
	reordered.Suites = append([]HistoricalSuiteBaseline(nil), registry.Suites...)
	reordered.Suites[0].Trail = []string{
		"z surviving owner note", "docs/measurement.md synthetic recorded baseline",
	}
	resealed, err := SealHistoricalBaselineRegistry(reordered, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(resealed.Suites[0].Trail[0], "docs/") {
		t.Fatalf("trail was not canonicalized: %v", resealed.Suites[0].Trail)
	}
}

func TestHistoricalBaselineRegistryRejectsFabricatedOrIncompleteAuthority(t *testing.T) {
	manifest := testManifest()
	valid := testHistoricalBaselineRegistry(t, manifest)
	tests := map[string]func(*HistoricalBaselineRegistry){
		"authority": func(registry *HistoricalBaselineRegistry) {
			registry.Authority = "inferred-by-runner"
		},
		"manifest": func(registry *HistoricalBaselineRegistry) {
			registry.ManifestID = strings.Repeat("9", 64)
		},
		"population": func(registry *HistoricalBaselineRegistry) {
			registry.Suites[0].ExpectedAttempts--
		},
		"trail": func(registry *HistoricalBaselineRegistry) {
			registry.Suites[0].Trail = nil
		},
		"pass denominator": func(registry *HistoricalBaselineRegistry) {
			registry.Suites[0].Pass.ApplicableAttempts = 7
		},
		"required interaction": func(registry *HistoricalBaselineRegistry) {
			registry.Suites[0].Interaction = nil
		},
		"required safety": func(registry *HistoricalBaselineRegistry) {
			registry.Suites[0].Safety = nil
		},
		"latency omitted": func(registry *HistoricalBaselineRegistry) {
			registry.Suites[0].Latencies = nil
		},
		"latency disorder": func(registry *HistoricalBaselineRegistry) {
			registry.Suites[0].Latencies[0].Distribution.P50 = 1000
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			payload, err := json.Marshal(valid)
			if err != nil {
				t.Fatal(err)
			}
			var candidate HistoricalBaselineRegistry
			if err := json.Unmarshal(payload, &candidate); err != nil {
				t.Fatal(err)
			}
			mutate(&candidate)
			candidate.RegistryID = candidate.ID()
			if err := candidate.ValidateAgainst(manifest); err == nil {
				t.Fatal("invalid historical authority was accepted")
			}
		})
	}
}

func TestHistoricalBaselineRegistryRequiresEveryGatedPartition(t *testing.T) {
	manifest, baseline, _ := partitionedConditionFixture()
	suite := manifest.Suites[0]
	latency := func(count int) []HistoricalLatency {
		values := make([]float64, count)
		for index := range values {
			values[index] = 100
		}
		return []HistoricalLatency{{Name: "response_latency_ms", Unit: "ms",
			ApplicableCases: count / 2,
			Distribution:    distribution(values, "ms")}}
	}
	rate := func(attempts, cases, successes int) HistoricalRate {
		return HistoricalRate{ApplicableAttempts: attempts, ApplicableCases: cases, Successes: successes}
	}
	condition := func(name string) HistoricalPartitionBaseline {
		interaction, deadline := rate(4, 2, 4), rate(4, 2, 4)
		return HistoricalPartitionBaseline{Condition: name, Pass: rate(4, 2, 3),
			Interaction: &interaction, Deadline: &deadline, Latencies: latency(4)}
	}
	interaction, deadline := rate(8, 4, 8), rate(8, 4, 8)
	registry, err := SealHistoricalBaselineRegistry(HistoricalBaselineRegistry{
		Version: HistoricalBaselineRegistryVersion, Authority: "benchmark-owner",
		Acceptance: "Owner accepted historical partition trail.", ManifestID: manifest.ID(),
		Suites: []HistoricalSuiteBaseline{{
			Suite: suite.Name, ExpectedCases: suite.ExpectedCases,
			ExpectedAttempts: suite.ExpectedAttempts, Trail: []string{"recorded partition trail"},
			Pass: rate(8, 4, 6), Interaction: &interaction, Deadline: &deadline,
			Safety:    &HistoricalSafety{ApplicableAttempts: 8, ApplicableCases: 4},
			Latencies: latency(8), Conditions: []HistoricalPartitionBaseline{
				condition("stable"), condition("improving"),
			}, Cases: []HistoricalPartitionBaseline{},
		}},
	}, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if registry.Suites[0].Conditions[0].Condition != "improving" {
		t.Fatalf("conditions were not canonicalized: %+v", registry.Suites[0].Conditions)
	}
	registry.Suites[0].Conditions = registry.Suites[0].Conditions[:1]
	registry.RegistryID = registry.ID()
	if err := registry.ValidateAgainst(manifest); err == nil {
		t.Fatal("missing gated condition was accepted")
	}
	_ = baseline
}

func TestDecodeHistoricalBaselineRegistryRejectsUnknownTrailingAndNoncanonicalJSON(t *testing.T) {
	manifest := testManifest()
	registry := testHistoricalBaselineRegistry(t, manifest)
	payload, err := MarshalHistoricalBaselineRegistry(registry, manifest)
	if err != nil {
		t.Fatal(err)
	}
	unknown := bytes.Replace(payload, []byte(`"authority":`), []byte(`"unknown":true,"authority":`), 1)
	for name, candidate := range map[string][]byte{
		"unknown":      unknown,
		"trailing":     append(append([]byte(nil), payload...), []byte("{}")...),
		"noncanonical": append([]byte(" \n"), payload...),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeHistoricalBaselineRegistry(bytes.NewReader(candidate), manifest); err == nil {
				t.Fatal("malformed registry was accepted")
			}
		})
	}
}
