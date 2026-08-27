package cascade

import (
	"testing"

	"github.com/bojieli/OpenRealtime/action"
)

func TestOneSolicitationAwaitsOneAnswer(t *testing.T) {
	runtime := &runtime{ledger: action.NewLedger()}
	if !runtime.claimSolicitation("first") {
		t.Fatal("the first solicitation was refused")
	}
	if runtime.claimSolicitation("second") {
		t.Fatal("a second solicitation was admitted before new user evidence")
	}
	runtime.clearSolicitation("")
	if !runtime.claimSolicitation("after-answer") {
		t.Fatal("new user evidence did not reopen solicitation")
	}
}

func TestOnlyUncrossedSolicitationMayBeReleasedAfterAnError(t *testing.T) {
	sessionRuntime := &runtime{ledger: action.NewLedger()}
	if !sessionRuntime.claimSolicitation("queued") {
		t.Fatal("the queued solicitation was refused")
	}
	if err := sessionRuntime.ledger.Prepare(action.Commitment{ID: "queued", Kind: action.KindSpeech}); err != nil {
		t.Fatalf("prepare queued solicitation: %v", err)
	}
	if err := sessionRuntime.ledger.Queue("queued"); err != nil {
		t.Fatalf("queue solicitation: %v", err)
	}
	sessionRuntime.clearUncrossedSolicitation("queued")
	if !sessionRuntime.claimSolicitation("replacement") {
		t.Fatal("an unheard failed solicitation kept the answer obligation")
	}

	crossed := &runtime{ledger: action.NewLedger()}
	if !crossed.claimSolicitation("crossed") {
		t.Fatal("the crossed solicitation was refused")
	}
	if err := crossed.ledger.Prepare(action.Commitment{ID: "crossed", Kind: action.KindText}); err != nil {
		t.Fatalf("prepare crossed solicitation: %v", err)
	}
	if err := crossed.ledger.Emit("crossed"); err != nil {
		t.Fatalf("emit solicitation: %v", err)
	}
	crossed.clearUncrossedSolicitation("crossed")
	if crossed.claimSolicitation("second") {
		t.Fatal("a text request that reached the user was released by a later sink error")
	}
}

func TestPoliteRequestCreatesAnAnswerObligationWithoutAQuestionMark(t *testing.T) {
	if !asksForReply("No problem, please spell your name clearly.") {
		t.Fatal("a polite imperative did not create an answer obligation")
	}
	if !asksForReply("Would you please confirm the order ID.") {
		t.Fatal("a reply-seeking modal without question punctuation was missed")
	}
	for _, direction := range []string{
		"Please hold on.",
		"Please note that exchanges take three days.",
		"Name and zip code are accepted instead.",
	} {
		if asksForReply(direction) {
			t.Fatalf("a direction or fact was mistaken for a question: %q", direction)
		}
	}
}
