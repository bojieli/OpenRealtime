package admission

import (
	"context"
	"strings"
	"testing"
	"time"
)

// validateRequest is the gate in front of every admission, and neither of its
// two refusals was exercised. The second one is the reservation itself: if it
// is deleted, a large background or speculative request is admitted into the
// capacity held back for interactive work, and interactive latency degrades
// with nothing failing anywhere.
func TestAdmissionRefusesUnsizedRequestsAndProtectsTheInteractiveReservation(t *testing.T) {
	t.Parallel()
	governor, err := NewGovernor(Config{Capacity: 10, ReservedInteractive: 4})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		request Request
		want    string
	}{
		{
			name:    "class is outside the declared vocabulary",
			request: Request{Class: Class(99), Cost: 1},
			want:    "invalid admission class",
		},
		{
			name:    "cost is zero",
			request: Request{Class: ClassInteractive, Cost: 0},
			want:    "cost must be between 1 and 10",
		},
		{
			name:    "cost is negative",
			request: Request{Class: ClassInteractive, Cost: -1},
			want:    "cost must be between 1 and 10",
		},
		{
			name:    "cost exceeds the whole domain",
			request: Request{Class: ClassUrgent, Cost: 11},
			want:    "cost must be between 1 and 10",
		},
		{
			name:    "background work reaches into the interactive reservation",
			request: Request{Class: ClassBackground, Cost: 7},
			want:    "outside the interactive reservation",
		},
		{
			name:    "speculative work reaches into the interactive reservation",
			request: Request{Class: ClassSpeculative, Cost: 7},
			want:    "outside the interactive reservation",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			// Bounded so that a deleted guard surfaces as a wrong error rather
			// than as a wait for capacity that can never exist.
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			lease, err := governor.Acquire(ctx, test.request)
			if lease != nil {
				lease.Release()
			}
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("acquire error = %v, want one containing %q", err, test.want)
			}
		})
	}

	// The boundary itself: background may fill exactly the unreserved capacity,
	// and interactive work may still use the whole domain. Each gets its own
	// governor, because these acquire real capacity and would otherwise queue
	// behind one another rather than testing the boundary.
	for _, test := range []struct {
		name    string
		request Request
	}{
		{"background fills exactly the unreserved capacity", Request{Class: ClassBackground, Cost: 6}},
		{"interactive may use the whole domain", Request{Class: ClassInteractive, Cost: 10}},
		{"urgent may use the whole domain", Request{Class: ClassUrgent, Cost: 10}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			admitted, err := NewGovernor(Config{Capacity: 10, ReservedInteractive: 4})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			lease, err := admitted.Acquire(ctx, test.request)
			if err != nil {
				t.Fatalf("acquire = %v, want admitted", err)
			}
			lease.Release()
		})
	}
}
