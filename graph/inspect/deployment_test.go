package inspect_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/graph/inspect"
)

func TestDeploymentEvidenceCanonicalRedactedCloneAndTraceReplay(t *testing.T) {
	evidence := inspect.DeploymentEvidence{
		Public: inspect.ArtifactIdentity{
			ID: "deployment://reviewed", Revision: "openrealtime.ai/deployment/v1alpha1",
			Digest: testDeploymentDigest("1"),
		},
		PrivateDeploymentFingerprint: testDeploymentDigest("2"),
		SecretCatalogFingerprint:     testDeploymentDigest("3"),
		Secrets: []inspect.SecretProviderEvidence{
			{Node: "z", Slot: "token", BindingFingerprint: testDeploymentDigest("5"),
				Provider: "vault", Runtime: inspect.ArtifactIdentity{
					ID: "provider://vault", Revision: "1", Digest: testDeploymentDigest("6"),
				}},
			{Node: "a", Slot: "key", BindingFingerprint: testDeploymentDigest("4"),
				Provider: "keychain", Runtime: inspect.ArtifactIdentity{
					ID: "provider://keychain", Revision: "1", Digest: testDeploymentDigest("7"),
				}},
		},
	}
	canonical, err := inspect.CanonicalDeploymentEvidence(evidence)
	if err != nil {
		t.Fatal(err)
	}
	if err := canonical.ValidateExact(); err != nil {
		t.Fatal(err)
	}
	if canonical.Secrets[0].Node != "a" || canonical.Secrets[1].Node != "z" {
		t.Fatalf("canonical secrets = %+v", canonical.Secrets)
	}
	clone := canonical.Clone()
	clone.Secrets[0].Provider = "mutated"
	if canonical.Secrets[0].Provider != "keychain" {
		t.Fatal("deployment evidence clone retained a caller alias")
	}

	graph, configuration, trace, _ := liveTraceFixture(t)
	trace.Deployment = &canonical
	trace, err = inspect.FreezeLiveTrace(trace)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := inspect.MarshalLiveTrace(trace)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		"secret://private/reference", "PRIVATE_PROVIDER_LOCATOR", "private-credential-bytes",
	} {
		if strings.Contains(string(payload), forbidden) {
			t.Fatalf("deployment trace leaked %q: %s", forbidden, payload)
		}
	}
	parsed, err := inspect.ParseLiveTrace(payload)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(
		string(payload), canonical.SecretCatalogFingerprint, testDeploymentDigest("8"), 1,
	)
	if tampered == string(payload) {
		t.Fatal("deployment trace tamper did not alter the artifact")
	}
	if _, err := inspect.ParseLiveTrace([]byte(tampered)); err == nil ||
		!strings.Contains(err.Error(), "fingerprint") {
		t.Fatalf("deployment trace tamper error = %v", err)
	}
	replayer, err := inspect.NewTraceReplayer(graph, parsed)
	if err != nil {
		t.Fatal(err)
	}
	final, err := replayer.Final()
	if err != nil {
		t.Fatal(err)
	}
	if final.Deployment == nil || !reflect.DeepEqual(*final.Deployment, canonical) {
		t.Fatalf("replayed deployment = %+v", final.Deployment)
	}
	final.Deployment.Secrets[0].Provider = "caller-mutation"
	again, err := replayer.Final()
	if err != nil {
		t.Fatal(err)
	}
	if again.Deployment.Secrets[0].Provider != "keychain" {
		t.Fatal("trace replay retained a caller deployment alias")
	}

	live := traceLiveView(graph, configuration, 1, "mounted", "mounted", "private")
	live.Deployment = &canonical
	if _, err := inspect.TraceSnapshotFromLiveWithDeployment(
		graph, configuration, &canonical, live, 10,
	); err != nil {
		t.Fatal(err)
	}
	drifted := canonical.Clone()
	drifted.SecretCatalogFingerprint = testDeploymentDigest("8")
	if _, err := inspect.TraceSnapshotFromLiveWithDeployment(
		graph, configuration, &drifted, live, 10,
	); err == nil || !strings.Contains(err.Error(), "differs") {
		t.Fatalf("deployment drift capture error = %v", err)
	}
}

func TestDeploymentEvidenceRejectsAdversarialIdentityAndCoverage(t *testing.T) {
	valid := inspect.DeploymentEvidence{
		Public: inspect.ArtifactIdentity{
			ID: "deployment://reviewed", Revision: "1", Digest: testDeploymentDigest("1"),
		},
		PrivateDeploymentFingerprint: testDeploymentDigest("2"),
		SecretCatalogFingerprint:     testDeploymentDigest("3"),
		Secrets: []inspect.SecretProviderEvidence{{
			Node: "node", Slot: "token", BindingFingerprint: testDeploymentDigest("4"),
			Provider: "vault", Runtime: inspect.ArtifactIdentity{
				ID: "provider://vault", Revision: "1", Digest: testDeploymentDigest("5"),
			},
		}},
	}
	tests := []struct {
		name   string
		mutate func(*inspect.DeploymentEvidence)
		want   string
	}{
		{name: "missing private", mutate: func(value *inspect.DeploymentEvidence) {
			value.PrivateDeploymentFingerprint = ""
		}, want: "private deployment"},
		{name: "uppercase digest", mutate: func(value *inspect.DeploymentEvidence) {
			value.SecretCatalogFingerprint = "sha256:" + strings.Repeat("A", 64)
		}, want: "canonical SHA-256"},
		{name: "missing catalog", mutate: func(value *inspect.DeploymentEvidence) {
			value.SecretCatalogFingerprint = ""
		}, want: "secret-catalog"},
		{name: "duplicate slot", mutate: func(value *inspect.DeploymentEvidence) {
			value.Secrets = append(value.Secrets, value.Secrets[0])
		}, want: "repeats"},
		{name: "private node text", mutate: func(value *inspect.DeploymentEvidence) {
			value.Secrets[0].Node = "node\nsecret://private/reference"
		}, want: "canonical"},
		{name: "empty provider runtime", mutate: func(value *inspect.DeploymentEvidence) {
			value.Secrets[0].Runtime = inspect.ArtifactIdentity{}
		}, want: "runtime"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := valid.Clone()
			test.mutate(&candidate)
			if err := candidate.ValidateExact(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validation error = %v, want %q", err, test.want)
			}
		})
	}

	payload, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{
		"secret://private/reference", "PRIVATE_PROVIDER_LOCATOR", "private-credential-bytes",
	} {
		if strings.Contains(string(payload), private) {
			t.Fatalf("deployment evidence schema leaked %q", private)
		}
	}
}

func FuzzDeploymentEvidenceRejectsUnsafeFingerprint(f *testing.F) {
	f.Add("sha256:"+strings.Repeat("a", 64), "node", "slot")
	f.Add("private", "node\nlocator", "slot")
	f.Fuzz(func(t *testing.T, fingerprint, node, slot string) {
		evidence := inspect.DeploymentEvidence{
			Public: inspect.ArtifactIdentity{
				ID: "deployment://fuzz", Revision: "1", Digest: testDeploymentDigest("1"),
			},
			PrivateDeploymentFingerprint: testDeploymentDigest("2"),
			SecretCatalogFingerprint:     testDeploymentDigest("3"),
			Secrets: []inspect.SecretProviderEvidence{{
				Node: node, Slot: slot, BindingFingerprint: fingerprint, Provider: "provider",
				Runtime: inspect.ArtifactIdentity{
					ID: "provider://fuzz", Revision: "1", Digest: testDeploymentDigest("4"),
				},
			}},
		}
		canonical, err := inspect.CanonicalDeploymentEvidence(evidence)
		if err != nil {
			return
		}
		if err := canonical.ValidateExact(); err != nil {
			t.Fatal(err)
		}
		payload, err := json.Marshal(canonical)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(payload), "secret://") {
			t.Fatal("canonical evidence gained a secret reference")
		}
	})
}

func BenchmarkCanonicalDeploymentEvidence(b *testing.B) {
	evidence := inspect.DeploymentEvidence{
		Public: inspect.ArtifactIdentity{
			ID: "deployment://benchmark", Revision: "1", Digest: testDeploymentDigest("1"),
		},
		PrivateDeploymentFingerprint: testDeploymentDigest("2"),
		SecretCatalogFingerprint:     testDeploymentDigest("3"),
	}
	for index := 255; index >= 0; index-- {
		evidence.Secrets = append(evidence.Secrets, inspect.SecretProviderEvidence{
			Node: "node-" + leftPaddedDecimal(index), Slot: "token",
			BindingFingerprint: testDeploymentDigest("4"), Provider: "provider",
			Runtime: inspect.ArtifactIdentity{
				ID: "provider://benchmark", Revision: "1", Digest: testDeploymentDigest("5"),
			},
		})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		if _, err := inspect.CanonicalDeploymentEvidence(evidence); err != nil {
			b.Fatal(err)
		}
	}
}

func testDeploymentDigest(digit string) string {
	return "sha256:" + strings.Repeat(digit, 64)
}

func leftPaddedDecimal(value int) string {
	result := []byte{'0', '0', '0'}
	for index := len(result) - 1; index >= 0; index-- {
		result[index] = byte('0' + value%10)
		value /= 10
	}
	return string(result)
}
