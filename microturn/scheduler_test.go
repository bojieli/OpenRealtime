package microturn

import "testing"

func TestFixedSchedulerUsesOnlyRevisionsAvailableAtTick(t *testing.T) {
	t.Parallel()
	scheduler, err := NewFixedScheduler(100)
	if err != nil {
		t.Fatal(err)
	}
	first, err := scheduler.Observe(Observation{AtNS: 250, RevisionID: 1, HasRevision: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || first[0].SourceRevisionID != 0 || first[1].OpenedNS != 200 {
		t.Fatalf("unexpected pre-revision ticks: %+v", first)
	}
	last, err := scheduler.Observe(Observation{AtNS: 375, Endpoint: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(last) != 2 || last[0].OpenedNS != 300 || last[0].SourceRevisionID != 1 || last[1].Reason != TriggerEndpoint {
		t.Fatalf("unexpected final opportunities: %+v", last)
	}
}

func TestFixedSchedulerRevisionAtExactTickIsVisible(t *testing.T) {
	t.Parallel()
	scheduler, err := NewFixedScheduler(100)
	if err != nil {
		t.Fatal(err)
	}
	opportunities, err := scheduler.Observe(Observation{AtNS: 100, RevisionID: 1, HasRevision: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(opportunities) != 1 || opportunities[0].SourceRevisionID != 1 {
		t.Fatalf("unexpected opportunity: %+v", opportunities)
	}
}

func TestEventSchedulerAndObservationInvariants(t *testing.T) {
	t.Parallel()
	scheduler := NewEventScheduler()
	opportunities, err := scheduler.Observe(Observation{AtNS: 10, RevisionID: 1, HasRevision: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(opportunities) != 1 || opportunities[0].Reason != TriggerPerceptionRevision {
		t.Fatalf("unexpected opportunities: %+v", opportunities)
	}
	if _, err := scheduler.Observe(Observation{AtNS: 9}); err == nil {
		t.Fatal("expected backwards observation to fail")
	}
}

func TestFixedSchedulerBoundsMissedOpportunityBatch(t *testing.T) {
	t.Parallel()
	scheduler, err := NewFixedScheduler(1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scheduler.Observe(Observation{AtNS: maxOpportunitiesPerObservation + 1}); err == nil {
		t.Fatal("expected oversized fixed opportunity batch to fail")
	}
}
