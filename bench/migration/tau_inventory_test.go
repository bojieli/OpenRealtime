package migration

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/bench/tauvoice"
)

func TestMigrationCensusRequiresCanonicalPartitionedTauInventory(t *testing.T) {
	inventory := tauvoice.TaskInventory{
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
			inventory.Tasks = append(inventory.Tasks, tauvoice.TaskIdentity{
				Domain: domain.name, ID: fmt.Sprintf("task-%03d", index),
			})
		}
	}
	payload, err := tauvoice.MarshalTaskInventory(inventory)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parseTauInventory(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Tasks) != tauvoice.TaskCount {
		t.Fatalf("parsed %d tasks", len(parsed.Tasks))
	}

	compact, err := json.Marshal(inventory)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseTauInventory(compact); err == nil || !strings.Contains(err.Error(), "canonical") {
		t.Fatalf("compact inventory error = %v", err)
	}

	wrongPartition := inventory
	wrongPartition.Tasks = append([]tauvoice.TaskIdentity(nil), inventory.Tasks...)
	wrongPartition.Tasks[0].Domain = "retail"
	wrongPayload, err := json.MarshalIndent(wrongPartition, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	wrongPayload = append(wrongPayload, '\n')
	if _, err := parseTauInventory(wrongPayload); err == nil || !strings.Contains(err.Error(), "retail") {
		t.Fatalf("wrong partition error = %v", err)
	}
}
