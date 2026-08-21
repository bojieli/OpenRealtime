package fdbv3

import (
	"encoding/json"
	"testing"
)

// The suite's whole reason for existing is that "did not know which tool to
// call" and "could not reassemble the identifier" are different failures. The
// scorer has to keep them different, and the second one is the easy one to
// score away by accident.

func TestSpelledIdentifiersAreReassembledOrTheyAreNot(t *testing.T) {
	for _, test := range []struct {
		name     string
		expected string
		observed string
		match    bool
	}{
		{
			name:     "exactly right",
			expected: `{"order_id":"BOB12"}`, observed: `{"order_id":"BOB12"}`, match: true,
		},
		{
			name:     "a hyphen the recogniser added is not a different order",
			expected: `{"order_id":"BOB12"}`, observed: `{"order_id":"BOB-12"}`, match: true,
		},
		{
			name:     "case is not a different order",
			expected: `{"order_id":"BOB12"}`, observed: `{"order_id":"bob12"}`, match: true,
		},
		{
			name: "letters never joined into a token is the failure this suite reports",
			// This is the one that matters. An agent that hears the caller
			// spell it out and sends the letters back unjoined has not got the
			// identifier, and a scorer that said otherwise would report the
			// project's own known failure mode as absent.
			expected: `{"order_id":"BOB12"}`, observed: `{"order_id":"B O B 1 2"}`, match: false,
		},
		{
			name:     "a partly joined identifier is still wrong",
			expected: `{"order_id":"BOB12"}`, observed: `{"order_id":"BO B12"}`, match: false,
		},
		{
			name:     "a real word boundary survives normalisation",
			expected: `{"passenger_name":"Morgan Smith"}`, observed: `{"passenger_name":"morgan  smith"}`, match: true,
		},
		{
			name:     "two words are not one word",
			expected: `{"passenger_name":"Morgan Smith"}`, observed: `{"passenger_name":"MorganSmith"}`, match: false,
		},
		{
			name:     "an extra optional argument is not a wrong call",
			expected: `{"order_id":"GG5"}`, observed: `{"order_id":"GG5","verbose":true}`, match: true,
		},
		{
			name:     "a missing argument is a wrong call",
			expected: `{"origin":"Oak Street","destination":"Gym"}`, observed: `{"origin":"Oak Street"}`, match: false,
		},
		{
			name:     "a number is compared by value as written",
			expected: `{"max_price":150}`, observed: `{"max_price":150}`, match: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := argumentsMatch(json.RawMessage(test.expected), json.RawMessage(test.observed))
			if got != test.match {
				t.Fatalf("expected match=%t for %s vs %s", test.match, test.expected, test.observed)
			}
		})
	}
}

// Which tool and which arguments are counted separately, so a report can say
// which of the two happened.
func TestScoreSeparatesTheCallFromItsArguments(t *testing.T) {
	expected := []ExpectedCall{{Function: "track_order", Args: json.RawMessage(`{"order_id":"BOB12"}`)}}

	names, arguments := score(expected, []observedCall{
		{Name: "track_order", Arguments: json.RawMessage(`{"order_id":"BOB12"}`)},
	})
	if names != 1 || arguments != 1 {
		t.Fatalf("a correct call must score both: names=%d arguments=%d", names, arguments)
	}

	names, arguments = score(expected, []observedCall{
		{Name: "track_order", Arguments: json.RawMessage(`{"order_id":"B O B 1 2"}`)},
	})
	if names != 1 || arguments != 0 {
		t.Fatalf("the right tool with the wrong identifier scores the tool only: names=%d arguments=%d",
			names, arguments)
	}

	names, arguments = score(expected, []observedCall{
		{Name: "cancel_order", Arguments: json.RawMessage(`{"order_id":"BOB12"}`)},
	})
	if names != 0 || arguments != 0 {
		t.Fatalf("the wrong tool scores nothing: names=%d arguments=%d", names, arguments)
	}

	// Given several attempts, the one that is right is the one that counts -
	// but it is only counted once.
	names, arguments = score(expected, []observedCall{
		{Name: "track_order", Arguments: json.RawMessage(`{"order_id":"WRONG"}`)},
		{Name: "track_order", Arguments: json.RawMessage(`{"order_id":"BOB12"}`)},
	})
	if names != 1 || arguments != 1 {
		t.Fatalf("a later correct attempt counts once: names=%d arguments=%d", names, arguments)
	}
}
