package interaction

import (
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/element"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
)

// validatePostCommitSilenceCommit is what stops a forged or incoherent commit
// from starting a post-commit silence window. Three of its refusals had no
// coverage, including the one that binds the envelope to the State item its
// context came from -- without which a commit could name a context prefix it
// never causally descended from.
func postCommitSilenceCommitFixture() (element.Envelope, stateelements.ObservationCommitOutcome) {
	envelope := postCommitSilenceEnvelope(3, "stream-a")
	return envelope, envelope.Payload.(stateelements.ObservationCommitOutcome)
}

func TestPostCommitSilenceCommitRefusesIncoherentEvidence(t *testing.T) {
	t.Parallel()
	envelope, commit := postCommitSilenceCommitFixture()
	if err := validatePostCommitSilenceCommit(envelope, commit); err != nil {
		t.Fatalf("coherent commit = %v, want accepted", err)
	}
	for _, test := range []struct {
		name string
		edit func(*element.Envelope, *stateelements.ObservationCommitOutcome)
		want string
	}{
		{
			name: "no store version",
			edit: func(_ *element.Envelope, commit *stateelements.ObservationCommitOutcome) {
				commit.StoreVersion = 0
				commit.Context.Prefix.Version = 0
			},
			want: "positive store, source, and observation revisions",
		},
		{
			name: "no source revision",
			edit: func(_ *element.Envelope, commit *stateelements.ObservationCommitOutcome) {
				commit.SourceRevision = 0
			},
			want: "positive store, source, and observation revisions",
		},
		{
			name: "no observation revision",
			edit: func(_ *element.Envelope, commit *stateelements.ObservationCommitOutcome) {
				commit.ObservationRevision = 0
			},
			want: "positive store, source, and observation revisions",
		},
		{
			// The committed context must be the store version it was taken
			// from, or the silence window is opened against another prefix.
			name: "context version differs from the store version",
			edit: func(_ *element.Envelope, commit *stateelements.ObservationCommitOutcome) {
				commit.Context.Prefix.Version = commit.StoreVersion + 1
			},
			want: "differs from store version",
		},
		{
			name: "context digest is not a SHA-256",
			edit: func(_ *element.Envelope, commit *stateelements.ObservationCommitOutcome) {
				commit.Context.Prefix.Digest = "sha256:short"
			},
			want: "not canonical SHA-256",
		},
		{
			name: "context digest is upper case",
			edit: func(_ *element.Envelope, commit *stateelements.ObservationCommitOutcome) {
				commit.Context.Prefix.Digest = "sha256:" + strings.Repeat("A", 64)
			},
			want: "not canonical SHA-256",
		},
		{
			name: "context digest is not hexadecimal",
			edit: func(_ *element.Envelope, commit *stateelements.ObservationCommitOutcome) {
				commit.Context.Prefix.Digest = "sha256:" + strings.Repeat("z", 64)
			},
			want: "not canonical SHA-256",
		},
		{
			name: "envelope does not causally name its context State item",
			edit: func(envelope *element.Envelope, _ *stateelements.ObservationCommitOutcome) {
				envelope.CausalParents = []string{"some-other-item"}
			},
			want: "does not causally name its context State item",
		},
		{
			name: "trigger item ID is not canonical",
			edit: func(_ *element.Envelope, commit *stateelements.ObservationCommitOutcome) {
				commit.TriggerItemID = " trigger"
			},
			want: "trigger item ID is not canonical",
		},
		{
			name: "session ID is not canonical",
			edit: func(envelope *element.Envelope, _ *stateelements.ObservationCommitOutcome) {
				envelope.SessionID = ""
			},
			want: "session ID is not canonical",
		},
		{
			name: "stream ID carries a control character",
			edit: func(_ *element.Envelope, commit *stateelements.ObservationCommitOutcome) {
				commit.StreamID = "stream\x01a"
			},
			want: "stream ID is not canonical",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			envelope, commit := postCommitSilenceCommitFixture()
			test.edit(&envelope, &commit)
			err := validatePostCommitSilenceCommit(envelope, commit)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("commit error = %v, want one containing %q", err, test.want)
			}
		})
	}
}
