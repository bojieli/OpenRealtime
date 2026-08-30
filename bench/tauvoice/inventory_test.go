package tauvoice_test

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/bench/tauvoice"
)

func TestTaskInventoryCanonicalRoundTrip(t *testing.T) {
	inventory := validTaskInventory()
	for left, right := 0, len(inventory.Tasks)-1; left < right; left, right = left+1, right-1 {
		inventory.Tasks[left], inventory.Tasks[right] = inventory.Tasks[right], inventory.Tasks[left]
	}
	payload, err := tauvoice.MarshalTaskInventory(inventory)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := tauvoice.DecodeTaskInventory(bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.Tasks) != tauvoice.TaskCount {
		t.Fatalf("decoded %d tasks", len(decoded.Tasks))
	}
	for index := 1; index < len(decoded.Tasks); index++ {
		previous, current := decoded.Tasks[index-1], decoded.Tasks[index]
		if previous.Domain > current.Domain ||
			(previous.Domain == current.Domain && previous.ID >= current.ID) {
			t.Fatalf("inventory is not strictly ordered at %d: %+v then %+v", index, previous, current)
		}
	}
	remarshaled, err := tauvoice.MarshalTaskInventory(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(payload, remarshaled) {
		t.Fatal("canonical task inventory bytes changed after round trip")
	}

	// Freeze owns its task slice; later mutation of the source must not alter
	// evidence already admitted into a migration census.
	frozen, err := tauvoice.FreezeTaskInventory(inventory)
	if err != nil {
		t.Fatal(err)
	}
	inventory.Tasks[0].ID = "mutated"
	if frozen.Tasks[0].ID == "mutated" {
		t.Fatal("frozen inventory aliases the caller's task slice")
	}
}

func TestTaskInventoryRejectsWrongUniverse(t *testing.T) {
	tests := []struct {
		name string
		edit func(*tauvoice.TaskInventory)
		want string
	}{
		{name: "version", edit: func(value *tauvoice.TaskInventory) { value.Version++ }, want: "version"},
		{name: "revision", edit: func(value *tauvoice.TaskInventory) { value.Revision = strings.Repeat("0", 40) }, want: "revision"},
		{name: "missing", edit: func(value *tauvoice.TaskInventory) { value.Tasks = value.Tasks[:len(value.Tasks)-1] }, want: "278"},
		{name: "duplicate", edit: func(value *tauvoice.TaskInventory) { value.Tasks[1] = value.Tasks[0] }, want: "repeats"},
		{name: "domain partition", edit: func(value *tauvoice.TaskInventory) { value.Tasks[0].Domain = "retail" }, want: "retail"},
		{name: "unknown domain", edit: func(value *tauvoice.TaskInventory) { value.Tasks[0].Domain = "banking" }, want: "unknown domain"},
		{name: "empty ID", edit: func(value *tauvoice.TaskInventory) { value.Tasks[0].ID = "" }, want: "empty"},
		{name: "padded ID", edit: func(value *tauvoice.TaskInventory) { value.Tasks[0].ID = " task" }, want: "whitespace"},
		{name: "control ID", edit: func(value *tauvoice.TaskInventory) { value.Tasks[0].ID = "task\nother" }, want: "control"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			inventory := validTaskInventory()
			testCase.edit(&inventory)
			if err := inventory.Validate(); err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("Validate() error = %v, want %q", err, testCase.want)
			}
		})
	}
}

func TestTaskInventoryDecoderIsStrictBoundedAndCanonical(t *testing.T) {
	payload, err := tauvoice.MarshalTaskInventory(validTaskInventory())
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		payload []byte
		want    string
	}{
		{name: "compact", payload: bytes.ReplaceAll(payload, []byte("\n"), nil), want: "canonical"},
		{name: "unknown", payload: bytes.Replace(payload, []byte("{\n"), []byte("{\n  \"unknown\": true,\n"), 1), want: "unknown"},
		{name: "trailing", payload: append(append([]byte(nil), payload...), []byte("{}\n")...), want: "trailing"},
		{name: "empty", payload: nil, want: "empty"},
		{name: "oversize", payload: []byte(strings.Repeat(" ", 600<<10)), want: "exceeds"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := tauvoice.DecodeTaskInventory(bytes.NewReader(testCase.payload)); err == nil ||
				!strings.Contains(strings.ToLower(err.Error()), strings.ToLower(testCase.want)) {
				t.Fatalf("DecodeTaskInventory() error = %v, want %q", err, testCase.want)
			}
		})
	}
	if _, err := tauvoice.DecodeTaskInventory(nil); err == nil || !strings.Contains(err.Error(), "nil") {
		t.Fatalf("nil reader error = %v", err)
	}
}

func validTaskInventory() tauvoice.TaskInventory {
	result := tauvoice.TaskInventory{
		Version:  tauvoice.TaskInventoryVersion,
		Revision: tauvoice.PinnedRevision,
	}
	for _, domain := range []struct {
		name  string
		count int
	}{
		{name: "airline", count: tauvoice.AirlineTaskCount},
		{name: "retail", count: tauvoice.RetailTaskCount},
		{name: "telecom", count: tauvoice.TelecomTaskCount},
	} {
		for index := 0; index < domain.count; index++ {
			result.Tasks = append(result.Tasks, tauvoice.TaskIdentity{
				Domain: domain.name,
				ID:     fmt.Sprintf("task-%03d", index),
			})
		}
	}
	return result
}
