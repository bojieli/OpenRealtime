package fdbv3

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const fixtureDispositionRegistryIdentity = "fdb-v3/fixture-dispositions-v2@" +
	pinnedReleasedRevision + "/artifact-sha256:" + pinnedReleasedArtifactDigest

type fixtureDispositionStatus string

const (
	fixtureStatusDeterminate     fixtureDispositionStatus = "determinate"
	fixtureStatusIndeterminate   fixtureDispositionStatus = "indeterminate_fixture"
	fixtureStatusContractInvalid fixtureDispositionStatus = "contract_invalid_fixture"
)

type fixtureDefectCode string

const (
	fixtureDefectAnnotationOnlyArgument  fixtureDefectCode = "annotation_argument_absent_from_callable"
	fixtureDefectArgumentTypeConflict    fixtureDefectCode = "annotation_argument_type_conflicts_with_callable"
	fixtureDefectMissingRequiredArgument fixtureDefectCode = "annotation_omits_required_callable_argument"
	fixtureDefectMissingResultPath       fixtureDefectCode = "annotation_result_path_absent_from_result"
	fixtureDefectUndefinedCondition      fixtureDefectCode = "annotation_condition_has_no_objective_predicate"
	fixtureDefectArgumentRequestMismatch fixtureDefectCode = "annotation_argument_disagrees_with_released_request"
	fixtureDefectArgumentUngrounded      fixtureDefectCode = "annotation_argument_not_grounded_in_released_request"
)

type fixtureDispositionKey struct {
	Revision       string
	ArtifactDigest string
	Task           string
}

type fixtureDefect struct {
	Code           fixtureDefectCode
	CallSlot       int
	Function       string
	Argument       string
	AnnotationJSON string
	AnnotationType string
	Reference      string
	Note           string
	RequestedJSON  string
	ReleasedUser   string
	AuditedAudio   string
	AudioSHA256    string
}

type fixtureDisposition struct {
	Status  fixtureDispositionStatus
	Defects []fixtureDefect
}

func pinnedFixtureDispositionKey(task string) fixtureDispositionKey {
	return fixtureDispositionKey{
		Revision: pinnedReleasedRevision, ArtifactDigest: pinnedReleasedArtifactDigest, Task: task,
	}
}

func contractInvalidFixture(defects ...fixtureDefect) fixtureDisposition {
	return fixtureDisposition{Status: fixtureStatusContractInvalid, Defects: defects}
}

func isReleasedRequestDefect(code fixtureDefectCode) bool {
	return code == fixtureDefectArgumentRequestMismatch || code == fixtureDefectArgumentUngrounded
}

// callableContractDisposition keeps Catalog validation usable for diagnostic,
// path-backed loads. Semantic request evidence is validated only by the
// released scorer, which owns immutable audio bytes; callable contradictions
// remain independently enforceable here.
func callableContractDisposition(disposition fixtureDisposition) fixtureDisposition {
	filtered := fixtureDisposition{Status: disposition.Status}
	for _, defect := range disposition.Defects {
		if !isReleasedRequestDefect(defect.Code) {
			filtered.Defects = append(filtered.Defects, defect)
		}
	}
	return filtered
}

// pinnedFixtureDispositions is the complete exception registry for the
// immutable released artifact. Every exception names an exact recording,
// expected-call slot, function, and contradictory field/value. There is no
// function-wide or path-pattern fallback: a future fixture cannot inherit one
// of these dispositions merely by using the same tool or argument name.
var pinnedFixtureDispositions = map[fixtureDispositionKey]fixtureDisposition{
	pinnedFixtureDispositionKey("ecommerce_12_6998abd731d2ec50d067d5bd"): contractInvalidFixture(
		fixtureDefect{Code: fixtureDefectAnnotationOnlyArgument, CallSlot: 0, Function: "search_products", Argument: "category", AnnotationJSON: `"electronics"`, AnnotationType: "string"},
		fixtureDefect{
			Code: fixtureDefectArgumentUngrounded, CallSlot: 0, Function: "search_products", Argument: "query", AnnotationJSON: `"gift"`,
			ReleasedUser: "Like... you know... uh... i'm thinking something in the... let me think... electronics section would be great. He's really into gadgets and tech stuff.",
			AuditedAudio: "electronics section would be great; he's really into gadgets and tech stuff",
			AudioSHA256:  "ad0f0a5a0076723ddbbfe4b2c9c1e9ac4ecd903e6532769ba68bd292d6cc041b",
		},
	),
	pinnedFixtureDispositionKey("ecommerce_21_69a9cf80f4d7668d5c815038"): contractInvalidFixture(
		fixtureDefect{
			Code: fixtureDefectArgumentRequestMismatch, CallSlot: 0, Function: "track_order", Argument: "order_id", AnnotationJSON: `"BOB"`, RequestedJSON: `"BOP"`,
			ReleasedUser: "Um... so hmm... hmm... well... could you track order B-O-B for me first? Then... um,... um, I also want to search for a nice watch under 200 dollars. And once you find something good, uh, go ahead and add it to my cart.",
			AuditedAudio: "could you track order B-O-P for me first",
			AudioSHA256:  "bdd1e7c41fa6626a379a3fad90704f70b7d74e299ff3aa6cd59a20301643b186",
		},
	),
	pinnedFixtureDispositionKey("finance_20_66c4f3cb14cbfc4db836bd4e"): {
		Status:  fixtureStatusIndeterminate,
		Defects: []fixtureDefect{{Code: fixtureDefectUndefinedCondition, CallSlot: 1, Function: "modify_autopay", Note: "Only if rate is favorable"}},
	},
	pinnedFixtureDispositionKey("housing_03_5f4a4da1575d605c43bef871"): contractInvalidFixture(
		fixtureDefect{Code: fixtureDefectArgumentTypeConflict, CallSlot: 0, Function: "update_search_filter", Argument: "value", AnnotationJSON: `1800`, AnnotationType: "number"},
	),
	pinnedFixtureDispositionKey("housing_04_695bd157114f0d2317f88617"): contractInvalidFixture(
		fixtureDefect{Code: fixtureDefectMissingRequiredArgument, CallSlot: 0, Function: "search_apartments", Argument: "max_price"},
	),
	pinnedFixtureDispositionKey("housing_05_5ff07b5ee7a1d23e719e421e"): contractInvalidFixture(
		fixtureDefect{Code: fixtureDefectMissingRequiredArgument, CallSlot: 0, Function: "search_apartments", Argument: "bedrooms"},
		fixtureDefect{Code: fixtureDefectAnnotationOnlyArgument, CallSlot: 0, Function: "search_apartments", Argument: "pets_allowed", AnnotationJSON: `true`, AnnotationType: "boolean"},
	),
	pinnedFixtureDispositionKey("housing_05_61517db6a7589569521b2356"): contractInvalidFixture(
		fixtureDefect{Code: fixtureDefectMissingRequiredArgument, CallSlot: 0, Function: "search_apartments", Argument: "bedrooms"},
		fixtureDefect{Code: fixtureDefectAnnotationOnlyArgument, CallSlot: 0, Function: "search_apartments", Argument: "pets_allowed", AnnotationJSON: `true`, AnnotationType: "boolean"},
	),
	pinnedFixtureDispositionKey("housing_08_66f59c766e7e22e1f90d08f6"): contractInvalidFixture(
		fixtureDefect{Code: fixtureDefectArgumentTypeConflict, CallSlot: 0, Function: "update_search_filter", Argument: "value", AnnotationJSON: `true`, AnnotationType: "boolean"},
	),
	pinnedFixtureDispositionKey("housing_11_62a885d5b6af18b3d4579e1b"): contractInvalidFixture(
		fixtureDefect{
			Code: fixtureDefectArgumentUngrounded, CallSlot: 0, Function: "search_apartments", Argument: "city", AnnotationJSON: `"Austin"`,
			ReleasedUser: "Uh... well... i was thinking a 1-bedroom under 1200... but actually, no wait — my partner is moving in with me, so we need a 2-bedroom, and we can go up... um, to 1600 a month.",
			AuditedAudio: "we need a two-bedroom, and we can go up to 1600 a month",
			AudioSHA256:  "d4b0a083891a617fc27497af4d1c7257f177fdc5049695d79f7b748af1af0b60",
		},
	),
	pinnedFixtureDispositionKey("housing_13_5f4a4da1575d605c43bef871"): contractInvalidFixture(
		fixtureDefect{Code: fixtureDefectArgumentTypeConflict, CallSlot: 0, Function: "update_search_filter", Argument: "value", AnnotationJSON: `3000`, AnnotationType: "number"},
		fixtureDefect{Code: fixtureDefectArgumentTypeConflict, CallSlot: 1, Function: "update_search_filter", Argument: "value", AnnotationJSON: `3`, AnnotationType: "number"},
	),
	pinnedFixtureDispositionKey("housing_14_5ff07b5ee7a1d23e719e421e"): contractInvalidFixture(
		fixtureDefect{Code: fixtureDefectAnnotationOnlyArgument, CallSlot: 0, Function: "search_apartments", Argument: "pets_allowed", AnnotationJSON: `true`, AnnotationType: "boolean"},
	),
	pinnedFixtureDispositionKey("housing_14_61517db6a7589569521b2356"): contractInvalidFixture(
		fixtureDefect{Code: fixtureDefectAnnotationOnlyArgument, CallSlot: 0, Function: "search_apartments", Argument: "pets_allowed", AnnotationJSON: `true`, AnnotationType: "boolean"},
	),
	pinnedFixtureDispositionKey("housing_15_5ff07b5ee7a1d23e719e421e"): contractInvalidFixture(
		fixtureDefect{Code: fixtureDefectMissingRequiredArgument, CallSlot: 0, Function: "search_apartments", Argument: "max_price"},
		fixtureDefect{Code: fixtureDefectAnnotationOnlyArgument, CallSlot: 0, Function: "search_apartments", Argument: "pets_allowed", AnnotationJSON: `true`, AnnotationType: "boolean"},
	),
	pinnedFixtureDispositionKey("housing_15_61517db6a7589569521b2356"): contractInvalidFixture(
		fixtureDefect{Code: fixtureDefectMissingRequiredArgument, CallSlot: 0, Function: "search_apartments", Argument: "max_price"},
		fixtureDefect{Code: fixtureDefectAnnotationOnlyArgument, CallSlot: 0, Function: "search_apartments", Argument: "pets_allowed", AnnotationJSON: `true`, AnnotationType: "boolean"},
	),
	pinnedFixtureDispositionKey("housing_18_66c4f3cb14cbfc4db836bd4e"): contractInvalidFixture(
		fixtureDefect{
			Code: fixtureDefectArgumentRequestMismatch, CallSlot: 0, Function: "search_apartments", Argument: "max_price", AnnotationJSON: `1800`, RequestedJSON: `800`,
			ReleasedUser: "Well... like... um... so first... search for a 1-bedroom in Portland that's under 1800 per month. Then once you find the cheapest option, check how long it would take to bike from there to the coffee shop on 5th Street where... um, I work.",
			AuditedAudio: "search for a one-bedroom in Portland that's under $800 per month",
			AudioSHA256:  "9f87a1514f7616179bb0fb3c1ba3cf62aca4ffe6cfa8f88247b116a96bf60bff",
		},
		fixtureDefect{Code: fixtureDefectMissingResultPath, CallSlot: 1, Function: "calculate_commute", Argument: "origin_address", Reference: "$RESULT_0.cheapest_apartment_address"},
	),
	pinnedFixtureDispositionKey("housing_20_62a885d5b6af18b3d4579e1b"): contractInvalidFixture(
		fixtureDefect{Code: fixtureDefectMissingRequiredArgument, CallSlot: 0, Function: "search_apartments", Argument: "bedrooms"},
		fixtureDefect{Code: fixtureDefectMissingResultPath, CallSlot: 1, Function: "calculate_commute", Argument: "origin_address", Reference: "$RESULT_0.apartments[0].address"},
		fixtureDefect{Code: fixtureDefectArgumentTypeConflict, CallSlot: 2, Function: "update_search_filter", Argument: "value", AnnotationJSON: `2500`, AnnotationType: "number"},
	),
	pinnedFixtureDispositionKey("housing_21_66c4f3cb14cbfc4db836bd4e"): contractInvalidFixture(
		fixtureDefect{Code: fixtureDefectMissingRequiredArgument, CallSlot: 0, Function: "search_apartments", Argument: "city"},
		fixtureDefect{Code: fixtureDefectMissingRequiredArgument, CallSlot: 0, Function: "search_apartments", Argument: "max_price"},
		fixtureDefect{Code: fixtureDefectMissingResultPath, CallSlot: 1, Function: "calculate_commute", Argument: "origin_address", Reference: "$RESULT_0.apartments[0].address"},
	),
	pinnedFixtureDispositionKey("housing_22_695bd157114f0d2317f88617"): contractInvalidFixture(
		fixtureDefect{Code: fixtureDefectArgumentTypeConflict, CallSlot: 0, Function: "update_search_filter", Argument: "value", AnnotationJSON: `true`, AnnotationType: "boolean"},
		fixtureDefect{Code: fixtureDefectMissingRequiredArgument, CallSlot: 1, Function: "search_apartments", Argument: "max_price"},
		fixtureDefect{Code: fixtureDefectMissingResultPath, CallSlot: 2, Function: "calculate_commute", Argument: "origin_address", Reference: "$RESULT_1.apartments[0].address"},
	),
	pinnedFixtureDispositionKey("housing_24_65e8cf8f4c7424fa062e54a3"): contractInvalidFixture(
		fixtureDefect{Code: fixtureDefectArgumentTypeConflict, CallSlot: 0, Function: "update_search_filter", Argument: "value", AnnotationJSON: `1500`, AnnotationType: "number"},
		fixtureDefect{Code: fixtureDefectMissingRequiredArgument, CallSlot: 1, Function: "search_apartments", Argument: "bedrooms"},
		fixtureDefect{Code: fixtureDefectMissingRequiredArgument, CallSlot: 1, Function: "search_apartments", Argument: "max_price"},
		fixtureDefect{Code: fixtureDefectMissingResultPath, CallSlot: 2, Function: "calculate_commute", Argument: "origin_address", Reference: "$RESULT_1.apartments[0].address"},
	),
	pinnedFixtureDispositionKey("housing_24_69a9cf80f4d7668d5c815038"): contractInvalidFixture(
		fixtureDefect{Code: fixtureDefectArgumentTypeConflict, CallSlot: 0, Function: "update_search_filter", Argument: "value", AnnotationJSON: `1500`, AnnotationType: "number"},
		fixtureDefect{Code: fixtureDefectMissingRequiredArgument, CallSlot: 1, Function: "search_apartments", Argument: "bedrooms"},
		fixtureDefect{Code: fixtureDefectMissingRequiredArgument, CallSlot: 1, Function: "search_apartments", Argument: "max_price"},
		fixtureDefect{Code: fixtureDefectMissingResultPath, CallSlot: 2, Function: "calculate_commute", Argument: "origin_address", Reference: "$RESULT_1.apartments[0].address"},
	),
	pinnedFixtureDispositionKey("housing_25_66f59c766e7e22e1f90d08f6"): contractInvalidFixture(
		fixtureDefect{Code: fixtureDefectArgumentTypeConflict, CallSlot: 0, Function: "update_search_filter", Argument: "value", AnnotationJSON: `true`, AnnotationType: "boolean"},
		fixtureDefect{Code: fixtureDefectArgumentTypeConflict, CallSlot: 1, Function: "update_search_filter", Argument: "value", AnnotationJSON: `3500`, AnnotationType: "number"},
		fixtureDefect{Code: fixtureDefectMissingRequiredArgument, CallSlot: 2, Function: "search_apartments", Argument: "bedrooms"},
		fixtureDefect{Code: fixtureDefectMissingRequiredArgument, CallSlot: 2, Function: "search_apartments", Argument: "max_price"},
	),
	pinnedFixtureDispositionKey("travel_02_5e3c1fbece3a7b000a6fd95a"): contractInvalidFixture(
		fixtureDefect{
			Code: fixtureDefectArgumentRequestMismatch, CallSlot: 0, Function: "update_identity_doc", Argument: "doc_number", AnnotationJSON: `"P9-9-9-90011"`, RequestedJSON: `"P88990011"`,
			ReleasedUser: "Uh, yeah, so my new passport number is P-8-8-9-9-0-0-1-1. Could you update that in the system... um, for me please?",
			AuditedAudio: "my new passport number is P-8-8-9-9-0-0-1-1",
			AudioSHA256:  "5d6926fe75fcec9c2ce07ecfe501278260f49efce76b49bca1a0d3b112da90f7",
		},
	),
}

func pinnedFixtureDisposition(
	taskID string, inventory releasedDatasetInventory,
) (fixtureDisposition, error) {
	if inventory.Revision != pinnedReleasedRevision ||
		inventory.ArtifactDigest != pinnedReleasedArtifactDigest {
		return fixtureDisposition{}, fmt.Errorf(
			"fixture disposition is unavailable for revision %q artifact %q",
			inventory.Revision, inventory.ArtifactDigest,
		)
	}
	index := sort.SearchStrings(pinnedReleasedInventory.TaskNames, taskID)
	if index >= len(pinnedReleasedInventory.TaskNames) ||
		pinnedReleasedInventory.TaskNames[index] != taskID {
		return fixtureDisposition{}, fmt.Errorf("fixture disposition is unavailable for unpinned task %q", taskID)
	}
	key := fixtureDispositionKey{
		Revision: inventory.Revision, ArtifactDigest: inventory.ArtifactDigest, Task: taskID,
	}
	if disposition, found := pinnedFixtureDispositions[key]; found {
		disposition.Defects = append([]fixtureDefect(nil), disposition.Defects...)
		return disposition, nil
	}
	return fixtureDisposition{Status: fixtureStatusDeterminate}, nil
}

func (disposition fixtureDisposition) reason() string {
	parts := make([]string, 0, len(disposition.Defects))
	for _, defect := range disposition.Defects {
		detail := fmt.Sprintf("%s at call %d %s", defect.Code, defect.CallSlot, defect.Function)
		switch {
		case defect.Reference != "":
			detail += " reference " + defect.Reference
		case defect.Note != "":
			detail += " note " + defect.Note
		case defect.RequestedJSON != "":
			detail += fmt.Sprintf(" argument %s annotation %s requested %s",
				defect.Argument, defect.AnnotationJSON, defect.RequestedJSON)
		case defect.Code == fixtureDefectArgumentUngrounded:
			detail += fmt.Sprintf(" argument %s annotation %s", defect.Argument, defect.AnnotationJSON)
		case defect.Argument != "":
			detail += " argument " + defect.Argument
		}
		parts = append(parts, detail)
	}
	return strings.Join(parts, "; ")
}

func pinnedAnnotationDefect(
	taskID string, callSlot int, function, argument string, code fixtureDefectCode,
	annotation json.RawMessage,
) (fixtureDefect, bool) {
	disposition, found := pinnedFixtureDispositions[pinnedFixtureDispositionKey(taskID)]
	if !found {
		return fixtureDefect{}, false
	}
	for _, defect := range disposition.Defects {
		if defect.Code != code || defect.CallSlot != callSlot || defect.Function != function ||
			defect.Argument != argument || defect.AnnotationJSON == "" {
			continue
		}
		if canonicalJSONEqual(annotation, defect.AnnotationJSON) {
			return defect, true
		}
	}
	return fixtureDefect{}, false
}

func pinnedMissingArgumentDefect(
	taskID string, callSlot int, function, argument string,
) (fixtureDefect, bool) {
	disposition, found := pinnedFixtureDispositions[pinnedFixtureDispositionKey(taskID)]
	if !found {
		return fixtureDefect{}, false
	}
	for _, defect := range disposition.Defects {
		if defect.Code == fixtureDefectMissingRequiredArgument && defect.CallSlot == callSlot &&
			defect.Function == function && defect.Argument == argument {
			return defect, true
		}
	}
	return fixtureDefect{}, false
}

func validateFixtureDispositionTask(task Task, disposition fixtureDisposition) error {
	seen := make(map[string]struct{}, len(disposition.Defects))
	for _, defect := range disposition.Defects {
		identity := fmt.Sprintf("%s/%d/%s/%s/%s/%s", defect.Code, defect.CallSlot,
			defect.Function, defect.Argument, defect.Reference, defect.Note)
		if _, duplicate := seen[identity]; duplicate {
			return fmt.Errorf("fixture disposition repeats defect %q", identity)
		}
		seen[identity] = struct{}{}
		if isReleasedRequestDefect(defect.Code) {
			if defect.AnnotationType != "" || defect.Reference != "" || defect.Note != "" {
				return fmt.Errorf("released-request defect %q mixes unrelated structural evidence", identity)
			}
		} else if defect.RequestedJSON != "" || defect.ReleasedUser != "" ||
			defect.AuditedAudio != "" || defect.AudioSHA256 != "" {
			return fmt.Errorf("structural defect %q mixes released-request evidence", identity)
		}
		if defect.CallSlot < 0 || defect.CallSlot >= len(task.Expected) {
			return fmt.Errorf("fixture disposition call slot %d is outside task %q", defect.CallSlot, task.ID)
		}
		call := task.Expected[defect.CallSlot]
		if call.Function != defect.Function {
			return fmt.Errorf("fixture disposition call %d names %q, task %q names %q",
				defect.CallSlot, defect.Function, task.ID, call.Function)
		}
		var arguments map[string]json.RawMessage
		if err := json.Unmarshal(call.Args, &arguments); err != nil {
			return fmt.Errorf("decode fixture disposition call %d arguments: %w", defect.CallSlot, err)
		}
		if arguments == nil {
			return fmt.Errorf("fixture disposition call %d arguments are not an object", defect.CallSlot)
		}
		switch defect.Code {
		case fixtureDefectAnnotationOnlyArgument, fixtureDefectArgumentTypeConflict:
			value, present := arguments[defect.Argument]
			if !present || !canonicalJSONEqual(value, defect.AnnotationJSON) {
				return fmt.Errorf("fixture disposition call %d %s.%s does not match exact annotation %s",
					defect.CallSlot, defect.Function, defect.Argument, defect.AnnotationJSON)
			}
			annotationType, err := jsonTypeOf(value)
			if err != nil || annotationType != defect.AnnotationType {
				return fmt.Errorf("fixture disposition call %d %s.%s type is %q, want %q",
					defect.CallSlot, defect.Function, defect.Argument, annotationType, defect.AnnotationType)
			}
		case fixtureDefectMissingRequiredArgument:
			if _, present := arguments[defect.Argument]; present {
				return fmt.Errorf("fixture disposition call %d %s.%s is not missing",
					defect.CallSlot, defect.Function, defect.Argument)
			}
		case fixtureDefectMissingResultPath:
			value, present := arguments[defect.Argument]
			if !present || !canonicalJSONEqual(value, mustJSONString(defect.Reference)) {
				return fmt.Errorf("fixture disposition call %d %s.%s does not contain exact reference %q",
					defect.CallSlot, defect.Function, defect.Argument, defect.Reference)
			}
		case fixtureDefectUndefinedCondition:
			if call.Note != defect.Note {
				return fmt.Errorf("fixture disposition call %d note is %q, want %q",
					defect.CallSlot, call.Note, defect.Note)
			}
		case fixtureDefectArgumentRequestMismatch, fixtureDefectArgumentUngrounded:
			value, present := arguments[defect.Argument]
			if !present || !canonicalJSONEqual(value, defect.AnnotationJSON) {
				return fmt.Errorf("fixture disposition call %d %s.%s does not match exact annotation %s",
					defect.CallSlot, defect.Function, defect.Argument, defect.AnnotationJSON)
			}
			if !taskHasExactReleasedUser(task, defect.ReleasedUser) {
				return fmt.Errorf("fixture disposition call %d %s.%s does not match exact released user text",
					defect.CallSlot, defect.Function, defect.Argument)
			}
			if strings.TrimSpace(defect.AuditedAudio) == "" ||
				strings.TrimSpace(defect.AudioSHA256) == "" || len(task.AudioWAV) == 0 {
				return fmt.Errorf("fixture disposition call %d %s.%s has incomplete audio evidence",
					defect.CallSlot, defect.Function, defect.Argument)
			}
			audioDigest := sha256.Sum256(task.AudioWAV)
			if fmt.Sprintf("%x", audioDigest) != defect.AudioSHA256 {
				return fmt.Errorf("fixture disposition call %d %s.%s audio digest does not match exact artifact",
					defect.CallSlot, defect.Function, defect.Argument)
			}
			switch defect.Code {
			case fixtureDefectArgumentRequestMismatch:
				if !json.Valid([]byte(defect.RequestedJSON)) ||
					canonicalJSONEqual(value, defect.RequestedJSON) {
					return fmt.Errorf("fixture disposition call %d %s.%s has invalid or non-disagreeing requested value %s",
						defect.CallSlot, defect.Function, defect.Argument, defect.RequestedJSON)
				}
				requested, err := scalarEvidenceText(defect.RequestedJSON)
				if err != nil || !strings.Contains(
					compactASCIIEvidence(defect.AuditedAudio), compactASCIIEvidence(requested),
				) {
					return fmt.Errorf("fixture disposition call %d %s.%s audio evidence does not contain requested value %s",
						defect.CallSlot, defect.Function, defect.Argument, defect.RequestedJSON)
				}
			case fixtureDefectArgumentUngrounded:
				if defect.RequestedJSON != "" {
					return fmt.Errorf("fixture disposition call %d %s.%s ungrounded argument unexpectedly has requested value %s",
						defect.CallSlot, defect.Function, defect.Argument, defect.RequestedJSON)
				}
				expected, err := scalarEvidenceText(defect.AnnotationJSON)
				if err != nil {
					return fmt.Errorf("fixture disposition call %d %s.%s expected evidence is not scalar: %w",
						defect.CallSlot, defect.Function, defect.Argument, err)
				}
				needle := compactASCIIEvidence(expected)
				if strings.Contains(compactASCIIEvidence(defect.ReleasedUser), needle) ||
					strings.Contains(compactASCIIEvidence(defect.AuditedAudio), needle) {
					return fmt.Errorf("fixture disposition call %d %s.%s expected value %s is present in released request evidence",
						defect.CallSlot, defect.Function, defect.Argument, defect.AnnotationJSON)
				}
			}
		default:
			return fmt.Errorf("fixture disposition uses unknown defect code %q", defect.Code)
		}
	}
	return nil
}

func taskHasExactReleasedUser(task Task, expected string) bool {
	for _, turn := range task.Dialogue {
		if turn.User == expected {
			return true
		}
	}
	return false
}

func scalarEvidenceText(raw string) (string, error) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return "", err
	}
	switch value := value.(type) {
	case string:
		return value, nil
	case json.Number:
		return value.String(), nil
	case bool:
		return fmt.Sprintf("%t", value), nil
	default:
		return "", fmt.Errorf("unsupported evidence value %T", value)
	}
}

func compactASCIIEvidence(value string) string {
	var compact strings.Builder
	for _, symbol := range strings.ToLower(value) {
		if symbol >= 'a' && symbol <= 'z' || symbol >= '0' && symbol <= '9' {
			compact.WriteRune(symbol)
		}
	}
	return compact.String()
}

func mustJSONString(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func canonicalJSONEqual(value json.RawMessage, expected string) bool {
	var left, right bytes.Buffer
	if json.Compact(&left, value) != nil || json.Compact(&right, []byte(expected)) != nil {
		return false
	}
	return bytes.Equal(left.Bytes(), right.Bytes())
}
