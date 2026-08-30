package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestMeetingHostRegistrationIsExplicitAndLiveVerified(t *testing.T) {
	executable := meetingProfileExecutable()
	lookupValue := ""
	lookupPresent := false
	verifier := meetingProfileVerifier()
	dependencies := meetingHostDeploymentDependencies{
		lookup: func(name string) (string, bool) {
			if name != meetingLocalDeploymentEnvironment {
				t.Fatalf("Meeting host looked up %q", name)
			}
			return lookupValue, lookupPresent
		},
		verifier: func() (meetingDeploymentVerifier, error) { return verifier, nil },
	}
	registration, err := newProductionServeMeetingRegistrationWithDependencies(
		context.Background(), executable, dependencies,
	)
	if err != nil || registration != nil || verifier.resolve != 0 {
		t.Fatalf("disabled Meeting host registration=%+v resolve=%d error=%v",
			registration, verifier.resolve, err)
	}

	lookupPresent, lookupValue = true, "yes"
	if _, err := newProductionServeMeetingRegistrationWithDependencies(
		context.Background(), executable, dependencies,
	); err == nil || !strings.Contains(err.Error(), "exactly 1") {
		t.Fatalf("invalid Meeting host opt-in error = %v", err)
	}

	lookupValue = "1"
	registration, err = newProductionServeMeetingRegistrationWithDependencies(
		context.Background(), executable, dependencies,
	)
	if err != nil {
		t.Fatal(err)
	}
	if registration == nil || registration.Application.Reference == "" ||
		verifier.resolve != 1 || verifier.verify == 0 {
		t.Fatalf("enabled Meeting host registration=%+v resolve=%d verify=%d",
			registration, verifier.resolve, verifier.verify)
	}
}

func TestMeetingHostRegistrationRejectsVerifierFailure(t *testing.T) {
	want := errors.New("live Meeting proof failed")
	verifier := meetingProfileVerifier()
	verifier.err = want
	_, err := newProductionServeMeetingRegistrationWithDependencies(
		context.Background(), meetingProfileExecutable(), meetingHostDeploymentDependencies{
			lookup: func(string) (string, bool) { return "1", true },
			verifier: func() (meetingDeploymentVerifier, error) {
				return verifier, nil
			},
		},
	)
	if !errors.Is(err, want) || !strings.Contains(err.Error(), "resolve") {
		t.Fatalf("Meeting host verifier error = %v", err)
	}
}
