package main

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

type benchmarkInvocationKind string

const (
	benchmarkInvocationAttempt    benchmarkInvocationKind = "attempt"
	benchmarkInvocationRecovery   benchmarkInvocationKind = "recovery"
	benchmarkInvocationReadOnly   benchmarkInvocationKind = "read_only"
	benchmarkInvocationDiagnostic benchmarkInvocationKind = "diagnostic"
	benchmarkInvocationBlocked    benchmarkInvocationKind = "blocked"
)

type benchmarkDispatch struct {
	canonical string
	kind      benchmarkInvocationKind
	run       func([]string, io.Writer) error
}

func resolveBenchmarkDispatch(alias string) (benchmarkDispatch, bool) {
	switch strings.ToLower(strings.TrimSpace(alias)) {
	case "execution", "attestation":
		return benchmarkDispatch{"execution", benchmarkInvocationReadOnly, runExecutionRequirement}, true
	case "architecture", "architecture-pair", "f52":
		return benchmarkDispatch{"architecture", benchmarkInvocationReadOnly, runArchitecturePair}, true
	case "fdb", "fdb-v1.5":
		return benchmarkDispatch{"fdb-v1.5", benchmarkInvocationAttempt, runFDB}, true
	case "fdbench", "fd-bench":
		return benchmarkDispatch{"fd-bench", benchmarkInvocationAttempt, runFDBench}, true
	case "fdbv3", "fdb-v3":
		return benchmarkDispatch{"fdb-v3", benchmarkInvocationAttempt, runFDBv3}, true
	case "tau-voice", "tauvoice", "tau":
		return benchmarkDispatch{"tau-voice", benchmarkInvocationAttempt, runTauVoice}, true
	case "realtime-cu", "realtime-computer-use", "computer-use":
		return benchmarkDispatch{"realtime-cu", benchmarkInvocationAttempt, runRealtimeCU}, true
	case "meeting", "meeting-assistant", "live-meeting":
		return benchmarkDispatch{"meeting", benchmarkInvocationAttempt, runMeeting}, true
	case "dynacu":
		return benchmarkDispatch{"dynacu", benchmarkInvocationAttempt, runDynaCU}, true
	case "review-candidate":
		return benchmarkDispatch{"review-candidate", benchmarkInvocationRecovery, runCandidateReviewRecovery}, true
	case "verify-candidate-review":
		return benchmarkDispatch{"verify-candidate-review", benchmarkInvocationReadOnly, runCandidateReviewVerification}, true
	default:
		return benchmarkDispatch{}, false
	}
}

func classifyBenchmarkInvocation(arguments []string) (benchmarkInvocationKind, error) {
	if len(arguments) == 0 {
		return "", errors.New("benchmark invocation has no suite")
	}
	dispatch, found := resolveBenchmarkDispatch(arguments[0])
	if !found {
		return "", fmt.Errorf("unknown benchmark suite %q", arguments[0])
	}
	flags := arguments[1:]
	switch dispatch.canonical {
	case "fd-bench":
		if enabled, err := benchmarkBooleanFlag(flags, "list"); err != nil {
			return "", err
		} else if enabled {
			return benchmarkInvocationReadOnly, nil
		}
	case "tau-voice":
		if len(flags) > 0 && strings.EqualFold(strings.TrimSpace(flags[0]), "inventory") {
			return benchmarkInvocationReadOnly, nil
		}
		if enabled, err := benchmarkBooleanFlag(flags, "verify"); err != nil {
			return "", err
		} else if enabled {
			return benchmarkInvocationReadOnly, nil
		}
	case "meeting", "realtime-cu":
		if enabled, err := benchmarkBooleanFlag(flags, "list"); err != nil {
			return "", err
		} else if enabled {
			return benchmarkInvocationReadOnly, nil
		}
		if enabled, err := benchmarkBooleanFlag(flags, "review-resume"); err != nil {
			return "", err
		} else if enabled {
			return benchmarkInvocationRecovery, nil
		}
	case "dynacu":
		if enabled, err := benchmarkBooleanFlag(flags, "verify"); err != nil {
			return "", err
		} else if enabled {
			return benchmarkInvocationReadOnly, nil
		}
	}
	return dispatch.kind, nil
}

func benchmarkBooleanFlag(arguments []string, name string) (bool, error) {
	value := false
	for _, argument := range arguments {
		trimmed := strings.TrimLeft(argument, "-")
		key, raw, assigned := strings.Cut(trimmed, "=")
		if key != name {
			continue
		}
		if !assigned {
			value = true
			continue
		}
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			return false, fmt.Errorf("invalid -%s value %q", name, raw)
		}
		value = parsed
	}
	return value, nil
}

func classifyTopLevelMeasurementInvocation(command string, arguments []string) (benchmarkInvocationKind, error) {
	switch strings.ToLower(strings.TrimSpace(command)) {
	case "bench":
		return classifyBenchmarkInvocation(arguments)
	case "scenario":
		return benchmarkInvocationAttempt, nil
	case "review":
		if len(arguments) == 0 {
			return "", errors.New("review invocation has no operation")
		}
		switch strings.ToLower(strings.TrimSpace(arguments[0])) {
		case "scenario":
			return benchmarkInvocationRecovery, nil
		case "verify-scenario":
			return benchmarkInvocationReadOnly, nil
		default:
			return "", fmt.Errorf("unknown review operation %q", arguments[0])
		}
	case "eval":
		// These produce decision/conversation diagnostics, never bench.Result or
		// a benchmark attempt lifecycle, so mandatory benchmark retention does
		// not silently change their semantics.
		return benchmarkInvocationDiagnostic, nil
	default:
		return "", fmt.Errorf("%q is not a measurement command", command)
	}
}
