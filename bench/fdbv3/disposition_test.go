package fdbv3

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestPinnedFixtureDispositionRegistryCoversExactReleasedPopulation(t *testing.T) {
	tasks, err := loadDataset(releasedDatasetRoot(t), 0, &pinnedReleasedInventory)
	if err != nil {
		t.Fatal(err)
	}
	wantAffected := map[string]fixtureDispositionStatus{
		"ecommerce_12_6998abd731d2ec50d067d5bd": fixtureStatusContractInvalid,
		"ecommerce_21_69a9cf80f4d7668d5c815038": fixtureStatusContractInvalid,
		"finance_20_66c4f3cb14cbfc4db836bd4e":   fixtureStatusIndeterminate,
		"housing_03_5f4a4da1575d605c43bef871":   fixtureStatusContractInvalid,
		"housing_04_695bd157114f0d2317f88617":   fixtureStatusContractInvalid,
		"housing_05_5ff07b5ee7a1d23e719e421e":   fixtureStatusContractInvalid,
		"housing_05_61517db6a7589569521b2356":   fixtureStatusContractInvalid,
		"housing_08_66f59c766e7e22e1f90d08f6":   fixtureStatusContractInvalid,
		"housing_11_62a885d5b6af18b3d4579e1b":   fixtureStatusContractInvalid,
		"housing_13_5f4a4da1575d605c43bef871":   fixtureStatusContractInvalid,
		"housing_14_5ff07b5ee7a1d23e719e421e":   fixtureStatusContractInvalid,
		"housing_14_61517db6a7589569521b2356":   fixtureStatusContractInvalid,
		"housing_15_5ff07b5ee7a1d23e719e421e":   fixtureStatusContractInvalid,
		"housing_15_61517db6a7589569521b2356":   fixtureStatusContractInvalid,
		"housing_18_66c4f3cb14cbfc4db836bd4e":   fixtureStatusContractInvalid,
		"housing_20_62a885d5b6af18b3d4579e1b":   fixtureStatusContractInvalid,
		"housing_21_66c4f3cb14cbfc4db836bd4e":   fixtureStatusContractInvalid,
		"housing_22_695bd157114f0d2317f88617":   fixtureStatusContractInvalid,
		"housing_24_65e8cf8f4c7424fa062e54a3":   fixtureStatusContractInvalid,
		"housing_24_69a9cf80f4d7668d5c815038":   fixtureStatusContractInvalid,
		"housing_25_66f59c766e7e22e1f90d08f6":   fixtureStatusContractInvalid,
		"travel_02_5e3c1fbece3a7b000a6fd95a":    fixtureStatusContractInvalid,
	}
	if len(tasks) != 100 || len(pinnedFixtureDispositions) != len(wantAffected) {
		t.Fatalf("population tasks=%d registry=%d affected=%d", len(tasks), len(pinnedFixtureDispositions), len(wantAffected))
	}

	gotAffected := make(map[string]fixtureDispositionStatus)
	determinate, indeterminate, contractInvalid := 0, 0, 0
	semanticStatuses := make(map[string]int)
	for _, task := range tasks {
		disposition, err := pinnedFixtureDisposition(task.ID, pinnedReleasedInventory)
		if err != nil {
			t.Fatalf("%s: %v", task.ID, err)
		}
		if err := validateFixtureDispositionTask(task, disposition); err != nil {
			t.Fatalf("%s disposition does not match exact artifact: %v", task.ID, err)
		}
		switch disposition.Status {
		case fixtureStatusDeterminate:
			determinate++
		case fixtureStatusIndeterminate:
			indeterminate++
			gotAffected[task.ID] = disposition.Status
		case fixtureStatusContractInvalid:
			contractInvalid++
			gotAffected[task.ID] = disposition.Status
		default:
			t.Fatalf("%s has unknown disposition %q", task.ID, disposition.Status)
		}
		semantic := scoreSemanticRepaired(task, nil, pinnedReleasedInventory)
		if semantic.Status == "" || semantic.Observed != 0 {
			t.Fatalf("%s silently disappeared from semantic population: %+v", task.ID, semantic)
		}
		semanticStatuses[semantic.Status]++
	}
	if determinate != 78 || indeterminate != 1 || contractInvalid != 21 ||
		!reflect.DeepEqual(gotAffected, wantAffected) {
		t.Fatalf("dispositions determinate=%d indeterminate=%d contract-invalid=%d\ngot=%v\nwant=%v",
			determinate, indeterminate, contractInvalid, gotAffected, wantAffected)
	}
	totalStatuses := 0
	for _, count := range semanticStatuses {
		totalStatuses += count
	}
	if totalStatuses != 100 || semanticStatuses[semanticStatusIndeterminateFixture] != 1 ||
		semanticStatuses[semanticStatusContractInvalid] != 21 {
		t.Fatalf("semantic status population = %v", semanticStatuses)
	}
}

func TestPinnedFixtureDispositionRegistryExactlyMatchesKnownStructuralContradictions(t *testing.T) {
	tasks, err := loadDataset(releasedDatasetRoot(t), 0, &pinnedReleasedInventory)
	if err != nil {
		t.Fatal(err)
	}
	registered := make([]string, 0)
	for key, disposition := range pinnedFixtureDispositions {
		if key.Revision != pinnedReleasedRevision || key.ArtifactDigest != pinnedReleasedArtifactDigest ||
			key.Task == "" || strings.ContainsAny(key.Task, "*?[]") {
			t.Fatalf("registry contains non-exact key %+v", key)
		}
		for _, defect := range disposition.Defects {
			if isReleasedRequestDefect(defect.Code) {
				continue
			}
			registered = append(registered, fixtureDefectFingerprint(key.Task, defect))
		}
	}
	detected := make([]string, 0)
	for _, task := range tasks {
		for _, defect := range detectKnownFixtureContradictions(t, task) {
			detected = append(detected, fixtureDefectFingerprint(task.ID, defect))
		}
	}
	sort.Strings(registered)
	sort.Strings(detected)
	if !reflect.DeepEqual(registered, detected) {
		t.Fatalf("exact fixture registry differs from detected contradictions\nregistered:\n%s\ndetected:\n%s",
			strings.Join(registered, "\n"), strings.Join(detected, "\n"))
	}
}

func TestPinnedRequestDispositionsMatchExactReleasedDialogueAndAudio(t *testing.T) {
	tasks, err := loadDataset(releasedDatasetRoot(t), 0, &pinnedReleasedInventory)
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]Task, len(tasks))
	for _, task := range tasks {
		byID[task.ID] = task
	}

	// Each AuditedAudio excerpt was independently checked against input.wav;
	// the per-recording digest makes that human audit fail closed on byte drift.
	// ReleasedUser is independently decoded from metadata.json by loadDataset.
	want := map[string][]fixtureDefect{
		"ecommerce_12_6998abd731d2ec50d067d5bd": {{
			Code: fixtureDefectArgumentUngrounded, CallSlot: 0, Function: "search_products", Argument: "query", AnnotationJSON: `"gift"`,
			ReleasedUser: "Like... you know... uh... i'm thinking something in the... let me think... electronics section would be great. He's really into gadgets and tech stuff.",
			AuditedAudio: "electronics section would be great; he's really into gadgets and tech stuff",
			AudioSHA256:  "ad0f0a5a0076723ddbbfe4b2c9c1e9ac4ecd903e6532769ba68bd292d6cc041b",
		}},
		"ecommerce_21_69a9cf80f4d7668d5c815038": {{
			Code: fixtureDefectArgumentRequestMismatch, CallSlot: 0, Function: "track_order", Argument: "order_id", AnnotationJSON: `"BOB"`, RequestedJSON: `"BOP"`,
			ReleasedUser: "Um... so hmm... hmm... well... could you track order B-O-B for me first? Then... um,... um, I also want to search for a nice watch under 200 dollars. And once you find something good, uh, go ahead and add it to my cart.",
			AuditedAudio: "could you track order B-O-P for me first",
			AudioSHA256:  "bdd1e7c41fa6626a379a3fad90704f70b7d74e299ff3aa6cd59a20301643b186",
		}},
		"housing_11_62a885d5b6af18b3d4579e1b": {{
			Code: fixtureDefectArgumentUngrounded, CallSlot: 0, Function: "search_apartments", Argument: "city", AnnotationJSON: `"Austin"`,
			ReleasedUser: "Uh... well... i was thinking a 1-bedroom under 1200... but actually, no wait — my partner is moving in with me, so we need a 2-bedroom, and we can go up... um, to 1600 a month.",
			AuditedAudio: "we need a two-bedroom, and we can go up to 1600 a month",
			AudioSHA256:  "d4b0a083891a617fc27497af4d1c7257f177fdc5049695d79f7b748af1af0b60",
		}},
		"housing_18_66c4f3cb14cbfc4db836bd4e": {{
			Code: fixtureDefectArgumentRequestMismatch, CallSlot: 0, Function: "search_apartments", Argument: "max_price", AnnotationJSON: `1800`, RequestedJSON: `800`,
			ReleasedUser: "Well... like... um... so first... search for a 1-bedroom in Portland that's under 1800 per month. Then once you find the cheapest option, check how long it would take to bike from there to the coffee shop on 5th Street where... um, I work.",
			AuditedAudio: "search for a one-bedroom in Portland that's under $800 per month",
			AudioSHA256:  "9f87a1514f7616179bb0fb3c1ba3cf62aca4ffe6cfa8f88247b116a96bf60bff",
		}},
		"travel_02_5e3c1fbece3a7b000a6fd95a": {{
			Code: fixtureDefectArgumentRequestMismatch, CallSlot: 0, Function: "update_identity_doc", Argument: "doc_number", AnnotationJSON: `"P9-9-9-90011"`, RequestedJSON: `"P88990011"`,
			ReleasedUser: "Uh, yeah, so my new passport number is P-8-8-9-9-0-0-1-1. Could you update that in the system... um, for me please?",
			AuditedAudio: "my new passport number is P-8-8-9-9-0-0-1-1",
			AudioSHA256:  "5d6926fe75fcec9c2ce07ecfe501278260f49efce76b49bca1a0d3b112da90f7",
		}},
	}

	got := make(map[string][]fixtureDefect)
	for key, disposition := range pinnedFixtureDispositions {
		for _, defect := range disposition.Defects {
			if isReleasedRequestDefect(defect.Code) {
				got[key.Task] = append(got[key.Task], defect)
			}
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("request-semantic fixture registry differs\ngot: %#v\nwant: %#v", got, want)
	}

	for taskID, defects := range want {
		task, found := byID[taskID]
		if !found {
			t.Fatalf("audited task %q is absent from released inventory", taskID)
		}
		for _, defect := range defects {
			if err := validateFixtureDispositionTask(task, contractInvalidFixture(defect)); err != nil {
				t.Fatalf("%s exact request evidence: %v", taskID, err)
			}
			digest := sha256.Sum256(task.AudioWAV)
			if got := fmt.Sprintf("%x", digest); got != defect.AudioSHA256 {
				t.Fatalf("%s audio digest = %s, want %s", taskID, got, defect.AudioSHA256)
			}
		}
	}
}

func TestPathBackedDiagnosticCatalogDoesNotPretendToValidateAudioEvidence(t *testing.T) {
	tasks, err := Load(releasedDatasetRoot(t), 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Catalog(tasks); err != nil {
		t.Fatalf("path-backed diagnostic catalog rejected callable-valid released tasks: %v", err)
	}
	for _, task := range tasks {
		if task.ID != "travel_02_5e3c1fbece3a7b000a6fd95a" {
			continue
		}
		disposition, err := pinnedFixtureDisposition(task.ID, pinnedReleasedInventory)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateFixtureDispositionTask(task, disposition); err == nil ||
			!strings.Contains(err.Error(), "incomplete audio evidence") {
			t.Fatalf("path-backed task claimed immutable semantic audio evidence: %v", err)
		}
		return
	}
	t.Fatal("travel_02 semantic fixture is absent from diagnostic load")
}

func TestReleasedRequestDispositionEvidenceFailsClosed(t *testing.T) {
	tasks, err := loadDataset(releasedDatasetRoot(t), 0, &pinnedReleasedInventory)
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]Task, len(tasks))
	for _, task := range tasks {
		byID[task.ID] = task
	}
	travel := byID["travel_02_5e3c1fbece3a7b000a6fd95a"]
	disposition := pinnedFixtureDispositions[pinnedFixtureDispositionKey(travel.ID)]
	var mismatch fixtureDefect
	for _, defect := range disposition.Defects {
		if defect.Code == fixtureDefectArgumentRequestMismatch {
			mismatch = defect
		}
	}
	if mismatch.Code == "" {
		t.Fatal("travel_02 request mismatch is absent")
	}
	assertRejected := func(name string, task Task, defect fixtureDefect) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			if err := validateFixtureDispositionTask(
				task, contractInvalidFixture(defect),
			); err == nil {
				t.Fatal("mutated request evidence was accepted")
			}
		})
	}

	mutated := mismatch
	mutated.RequestedJSON = mutated.AnnotationJSON
	assertRejected("requested value no longer disagrees", travel, mutated)
	mutated = mismatch
	mutated.ReleasedUser += " changed"
	assertRejected("released dialogue changed", travel, mutated)
	mutated = mismatch
	mutated.AuditedAudio = "unrelated audio"
	assertRejected("audited excerpt lost requested value", travel, mutated)
	mutated = mismatch
	mutated.AudioSHA256 = strings.Repeat("0", sha256.Size*2)
	assertRejected("audio bytes changed", travel, mutated)
	mutatedTask := travel
	mutatedTask.AudioWAV = append([]byte(nil), travel.AudioWAV...)
	mutatedTask.AudioWAV[len(mutatedTask.AudioWAV)-1] ^= 1
	assertRejected("audio digest changed", mutatedTask, mismatch)

	ecommerce := byID["ecommerce_12_6998abd731d2ec50d067d5bd"]
	disposition = pinnedFixtureDispositions[pinnedFixtureDispositionKey(ecommerce.ID)]
	var ungrounded fixtureDefect
	for _, defect := range disposition.Defects {
		if defect.Code == fixtureDefectArgumentUngrounded {
			ungrounded = defect
		}
	}
	if ungrounded.Code == "" {
		t.Fatal("ecommerce_12 ungrounded argument is absent")
	}
	ungrounded.AuditedAudio += " gift"
	assertRejected("ungrounded value appeared in audio", ecommerce, ungrounded)
}

func TestFixtureExceptionsCannotBeClaimedByFutureTasks(t *testing.T) {
	for _, testCase := range []struct {
		name string
		call ExpectedCall
	}{
		{"annotation-only product category", ExpectedCall{Function: "search_products", Args: json.RawMessage(`{"query":"gift","category":"electronics"}`)}},
		{"non-string filter", ExpectedCall{Function: "update_search_filter", Args: json.RawMessage(`{"filter_name":"max_price","value":1800}`)}},
		{"missing apartment budget", ExpectedCall{Function: "search_apartments", Args: json.RawMessage(`{"city":"Denver","bedrooms":1}`)}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := Catalog([]Task{{ID: "future_recording", Expected: []ExpectedCall{testCase.call}}}); err == nil {
				t.Fatalf("future task inherited fixture exception: %v", err)
			}
		})
	}
}

func fixtureDefectFingerprint(task string, defect fixtureDefect) string {
	return fmt.Sprintf("%s|%s|%d|%s|%s|%s|%s", task, defect.Code, defect.CallSlot,
		defect.Function, defect.Argument, compactJSON(defect.AnnotationJSON), defect.Reference+defect.Note)
}

func compactJSON(value string) string {
	if value == "" {
		return ""
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(value)); err != nil {
		return "<invalid>" + value
	}
	return compact.String()
}

func detectKnownFixtureContradictions(t *testing.T, task Task) []fixtureDefect {
	t.Helper()
	var defects []fixtureDefect
	for callSlot, call := range task.Expected {
		contract := pinnedCallableContracts[call.Function]
		var arguments map[string]json.RawMessage
		if err := json.Unmarshal(call.Args, &arguments); err != nil {
			t.Fatalf("%s call %d: %v", task.ID, callSlot, err)
		}
		for argument, raw := range arguments {
			want, callable := callableArgumentType(contract, argument)
			if !callable {
				annotationType, err := jsonTypeOf(raw)
				if err != nil {
					t.Fatal(err)
				}
				defects = append(defects, fixtureDefect{
					Code: fixtureDefectAnnotationOnlyArgument, CallSlot: callSlot,
					Function: call.Function, Argument: argument,
					AnnotationJSON: string(raw), AnnotationType: annotationType,
				})
				continue
			}
			if err := validateCatalogAnnotationType(argument, want, raw, true); err != nil {
				annotationType, typeErr := jsonTypeOf(raw)
				if typeErr != nil {
					t.Fatal(typeErr)
				}
				defects = append(defects, fixtureDefect{
					Code: fixtureDefectArgumentTypeConflict, CallSlot: callSlot,
					Function: call.Function, Argument: argument,
					AnnotationJSON: string(raw), AnnotationType: annotationType,
				})
			}
			var reference string
			if json.Unmarshal(raw, &reference) == nil &&
				(strings.Contains(reference, ".apartments[") || strings.Contains(reference, ".cheapest_apartment_address")) {
				defects = append(defects, fixtureDefect{
					Code: fixtureDefectMissingResultPath, CallSlot: callSlot,
					Function: call.Function, Argument: argument, Reference: reference,
				})
			}
		}
		for _, argument := range contract.arguments {
			if argument.required {
				if _, present := arguments[argument.name]; !present {
					defects = append(defects, fixtureDefect{
						Code: fixtureDefectMissingRequiredArgument, CallSlot: callSlot,
						Function: call.Function, Argument: argument.name,
					})
				}
			}
		}
		if call.Note == "Only if rate is favorable" {
			defects = append(defects, fixtureDefect{
				Code: fixtureDefectUndefinedCondition, CallSlot: callSlot,
				Function: call.Function, Note: call.Note,
			})
		}
	}
	return defects
}
