package runtime

import "testing"

func TestMountServicesAreScopedAndDoNotMutateDeploymentServices(t *testing.T) {
	deployment := NewServiceSet()
	if _, err := deployment.Set("deployment.model", "fast"); err != nil {
		t.Fatal(err)
	}
	if _, err := deployment.Set(ClockServiceName, ClockFunc(func() uint64 { return 99 })); err != nil {
		t.Fatal(err)
	}

	first := newMountServices(deployment, func() uint64 { return 10 })
	second := newMountServices(deployment, func() uint64 { return 20 })
	for name, services := range map[string]anyServices{"first": first, "second": second} {
		if model, _, found := services.Lookup("deployment.model"); !found || model != "fast" {
			t.Fatalf("%s deployment lookup = %#v, found %t", name, model, found)
		}
	}

	firstClock, _, _ := first.Lookup(ClockServiceName)
	secondClock, _, _ := second.Lookup(ClockServiceName)
	if firstClock.(Clock).NowNS() != 10 || secondClock.(Clock).NowNS() != 20 {
		t.Fatalf("mount clocks leaked: first=%d second=%d",
			firstClock.(Clock).NowNS(), secondClock.(Clock).NowNS())
	}
	firstSequence, _, _ := first.Lookup(SequenceServiceName)
	secondSequence, _, _ := second.Lookup(SequenceServiceName)
	if next, err := firstSequence.(*SequenceAllocator).Next("revision"); err != nil || next != 1 {
		t.Fatalf("first sequence = %d, %v", next, err)
	}
	if next, err := secondSequence.(*SequenceAllocator).Next("revision"); err != nil || next != 1 {
		t.Fatalf("second sequence = %d, %v", next, err)
	}

	deploymentClock, _, _ := deployment.Lookup(ClockServiceName)
	if deploymentClock.(Clock).NowNS() != 99 {
		t.Fatalf("deployment service was mutated: %d", deploymentClock.(Clock).NowNS())
	}
}

type anyServices interface {
	Lookup(string) (any, uint64, bool)
}
