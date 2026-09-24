// Command interactionstudy runs annotated policy and paced audio diagnostics on
// the local GPU. Completed playback provides a text estimate, not word alignment
// or a validated paired capability score.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/adapters/speechsocket"
	"github.com/bojieli/OpenRealtime/bench/capability"
)

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0644)
}

func command(name string, args ...string) string {
	data, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Sprintf("%s: %v", data, err)
	}
	return strings.TrimSpace(string(data))
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	out := flag.String("out", "", "new immutable artifact directory (required)")
	audit := flag.String("audit-run", "", "offline v2 causal trace audit; out is a new report file")
	cellsFlag := flag.String("cells", "A1,A2,A3", "comma-separated cells")
	pairsFlag := flag.String("pairs", "st-01", "comma-separated pilot pairs")
	url := flag.String("url", "http://127.0.0.1:9100/v1", "local policy endpoint")
	model := flag.String("model", "qwen3-8b", "served model ID")
	repeats := flag.Int("repeats", 1, "repeat count")
	seed := flag.Int("seed", 1729, "first seed")
	speechText := flag.String("speech-text", "", "run isolated paced synthesis probe with this text")
	ttsURL := flag.String("tts-url", "ws://127.0.0.1:9125/v1/tts/stream", "speech service")
	controls := flag.Bool("live-controls", true, "include matched feedback-withheld live branches")
	live := flag.Bool("live", false, "wall-clock annotated diagnostic with paced speech playback")
	prepare := flag.String("prepare-pair", "", "synthesize a pair with identical shared-prefix audio")
	prepared := flag.String("prepared-pair", "", "prepared pair.json with sibling input WAVs")
	promptFile := flag.String("policy-prompt", "", "explicit experimental prompt file; default retains JointPrompt")
	omitHistory := flag.Bool("omit-policy-history", false, "live diagnostic: omit past decisions while retaining observations and playback state")
	affordance := flag.String("pending-affordance", "v1", "live: model-visible pending-segment affordance wording (v1 baseline, v2 wait-to-play, v3 state-specific, v4 pending-only change)")
	flag.Parse()
	if _, ok := pendingAffordances[*affordance]; !ok {
		return fmt.Errorf("unknown pending-affordance %q", *affordance)
	}
	if *omitHistory && !*live {
		return fmt.Errorf("omit-policy-history requires live mode")
	}
	if *audit != "" {
		return auditLive(*audit, *out)
	}
	if *prepare != "" {
		pair, ok := capability.PairByID(*prepare)
		if !ok || *out == "" {
			return fmt.Errorf("valid prepare-pair and out required")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		return preparePair(ctx, *out, pair, speechsocket.Config{URL: *ttsURL, Voice: "expresso/ex03-ex01_happy_001_channel1_334s.wav"})
	}
	if *speechText != "" {
		if *out == "" {
			return fmt.Errorf("out required")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		return renderSpeech(ctx, speechsocket.Config{URL: *ttsURL, Voice: "expresso/ex03-ex01_happy_001_channel1_334s.wav"}, *speechText, *out)
	}
	studyPairs := capability.Pilot()
	if *prepared != "" {
		raw, err := os.ReadFile(*prepared)
		if err != nil {
			return err
		}
		var pair capability.Pair
		if err = json.Unmarshal(raw, &pair); err != nil {
			return err
		}
		if err = pair.Validate(); err != nil {
			return err
		}
		studyPairs = []capability.Pair{pair}
		*pairsFlag = pair.ID
	}
	lookupPair := func(id string) (capability.Pair, bool) {
		for _, p := range studyPairs {
			if p.ID == id {
				return p, true
			}
		}
		return capability.Pair{}, false
	}
	if *out == "" || *repeats < 1 {
		return fmt.Errorf("out and positive repeats required")
	}
	if err := os.Mkdir(*out, 0755); err != nil {
		return err
	}
	// Freeze the source used, including untracked source files, before inference.
	sourceDir := filepath.Join(*out, "source")
	if err := os.Mkdir(sourceDir, 0755); err != nil {
		return err
	}
	hashes := map[string]string{}
	for _, dir := range []string{"bench/capability", "tools/interactionstudy"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			digest := sha256.Sum256(data)
			hashes[path] = hex.EncodeToString(digest[:])
			if err = os.WriteFile(filepath.Join(sourceDir, strings.ReplaceAll(path, "/", "__")+".txt"), data, 0644); err != nil {
				return err
			}
		}
	}
	manifest, err := os.ReadFile("docs/experiments/interaction-capability-v1.yaml")
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(*out, "experiment.yaml"), manifest, 0644); err != nil {
		return err
	}
	invocation := map[string]any{
		"playback_state_version": 2, "pending_affordance_version": *affordance, "live": *live, "omit_policy_history": *omitHistory, "live_controls": *controls, "cells": *cellsFlag,
		"pairs": *pairsFlag, "repeats": *repeats, "seed": *seed,
		"policy_model": *model, "policy_prompt_file": *promptFile, "max_tokens": 128, "tick_ms": 500,
		"prepared_pair": *prepared,
	}
	if *prepared != "" {
		raw, err := os.ReadFile(*prepared)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(raw)
		invocation["prepared_pair_sha256"] = hex.EncodeToString(digest[:])
		// Retain original bytes as well as decoded fixtures.
		if err = os.WriteFile(filepath.Join(*out, "prepared-pair.json"), raw, 0644); err != nil {
			return err
		}
		metadata, err := os.ReadFile(filepath.Join(filepath.Dir(*prepared), "recordings.json"))
		if err != nil {
			return err
		}
		if err = os.WriteFile(filepath.Join(*out, "recordings.json"), metadata, 0644); err != nil {
			return err
		}
	}
	if err = writeJSON(filepath.Join(*out, "invocation.json"), invocation); err != nil {
		return err
	}
	env := map[string]any{
		"started": time.Now().UTC(), "revision": command("git", "rev-parse", "HEAD"),
		"worktree_status": command("git", "status", "--short"),
		"source_sha256":   hashes, "gpu": command("nvidia-smi", "--query-gpu=name,memory.used,memory.free,utilization.gpu", "--format=csv"),
		"load": command("uptime"), "model": *model,
		"models_response": command("curl", "-fsS", "--max-time", "5", *url+"/models"),
	}
	if err := writeJSON(filepath.Join(*out, "environment.json"), env); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(*out, "fixtures.json"), studyPairs); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(*out, "cells.json"), capability.Cells()); err != nil {
		return err
	}
	if err := capability.ValidatePairs(studyPairs); err != nil {
		return err
	}
	policy := capability.JointPolicy{URL: *url, Model: *model, MaxTokens: 128}
	if *promptFile != "" {
		raw, err := os.ReadFile(*promptFile)
		if err != nil {
			return err
		}
		policy.Prompt = string(raw)
		if err = os.WriteFile(filepath.Join(*out, "policy-prompt.txt"), raw, 0644); err != nil {
			return err
		}
	}

	if *live {
		failedTrials := 0
		for _, id := range strings.Split(*pairsFlag, ",") {
			pair, ok := lookupPair(id)
			if !ok {
				return fmt.Errorf("unknown pair %s", id)
			}
			for _, cellID := range strings.Split(*cellsFlag, ",") {
				cell, err := capability.CellByID(cellID)
				if err != nil {
					return err
				}
				if !slices.Contains([]string{"A1", "A2", "A3", "A2D", "A2T"}, cellID) {
					return fmt.Errorf("live diagnostic implements A1-A3, A2D and A2T")
				}
				for repeat := 0; repeat < *repeats; repeat++ {
					policy.Seed = *seed + repeat
					variants := append([]capability.Variant(nil), pair.Variants...)
					if *controls {
						for _, v := range pair.Variants {
							control, e := pair.WithoutFeedback(v)
							if e != nil {
								return e
							}
							variants = append(variants, control)
						}
					}
					for _, variant := range variants {
						dir := filepath.Join(*out, fmt.Sprintf("%s-%s-%s-%d", cellID, id, variant.ID, repeat))
						ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
						err = runLive(ctx, dir, pair, variant, cell, policy, speechsocket.Config{URL: *ttsURL, Voice: "expresso/ex03-ex01_happy_001_channel1_334s.wav"}, *omitHistory, *affordance)
						cancel()
						if err != nil {
							failedTrials++
							fmt.Fprintf(os.Stderr, "%s: %v\n", dir, err)
						}
						if *prepared != "" {
							if err = attachInput(dir, filepath.Dir(*prepared), pair, variant); err != nil {
								return err
							}
						}
						fmt.Println(dir)
					}
				}
			}
		}
		if failedTrials > 0 {
			return fmt.Errorf("%d live trials failed; retained evidence in %s", failedTrials, *out)
		}
		return nil
	}

	failures, attempts := 0, 0
	for repeat := 0; repeat < *repeats; repeat++ {
		policy.Seed = *seed + repeat
		for _, pairID := range strings.Split(*pairsFlag, ",") {
			pair, ok := lookupPair(pairID)
			if !ok {
				return fmt.Errorf("unknown pair %s", pairID)
			}
			// Rotate cell order per repeat without concurrent GPU requests.
			ids := strings.Split(*cellsFlag, ",")
			for c := range ids {
				cell, err := capability.CellByID(ids[(c+repeat)%len(ids)])
				if err != nil {
					return err
				}
				if cell.ID != "A1" && cell.ID != "A2" && cell.ID != "A3" {
					return fmt.Errorf("diagnostic only implements A1-A3")
				}
				for _, variant := range pair.Variants {
					for _, control := range []bool{false, true} {
						branch := variant
						if control {
							branch, err = pair.WithoutFeedback(variant)
							if err != nil {
								return err
							}
						}
						dir := filepath.Join(*out, "runs", cell.ID, pair.ID, branch.ID, fmt.Sprint(repeat))
						if err = os.MkdirAll(dir, 0755); err != nil {
							return err
						}
						file, err := os.Create(filepath.Join(dir, "model-inputs-actions.jsonl"))
						if err != nil {
							return err
						}
						encoder := json.NewEncoder(file)
						source := capability.NewSource(pair.Branch(branch), cell.Channels, capability.DefaultDelays)
						listener := capability.NewListener(cell.Channels)
						// Virtual time diagnostic: requests run serially to completion.
						// Never report these durations as real-time audible latency.
						end := pair.PrefixEnd() + 20*time.Second
						if source.Ends()+3*time.Second > end {
							end = source.Ends() + 3*time.Second
						}
						var self capability.Self
						var history []capability.Action
						admitted := []capability.Admission{}
						runFailures := 0
						for now := time.Duration(0); now <= end; now += cell.TickInterval {
							fresh := source.Advance(now)
							admitted = append(admitted, fresh...)
							listener.Admit(fresh)
							observation := listener.Observe(now)
							rendered := cell.Render(observation, self)
							if len(history) > 0 {
								data, _ := json.Marshal(history)
								rendered += "\nEarlier decisions (generated text is NOT heard speech):\n" + string(data)
							}
							ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
							decision, callErr := policy.Decide(ctx, pair.Instructions, rendered)
							cancel()
							row := map[string]any{"pair_id": pair.ID, "cell_id": cell.ID, "variant_id": branch.ID,
								"repeat": repeat, "model_admission_ns": now, "decision": decision,
								"execution_mode": "virtual-time-text-only", "playback_mark": "unheard"}
							if err = encoder.Encode(row); err != nil {
								file.Close()
								return err
							}
							attempts++
							if callErr != nil {
								failures++
								runFailures++
								// Retain failure and finish this attempt. Do not retry selectively.
								break
							}
							history = append(history, decision.Action)
							if decision.Action.Text != "" {
								self.Pending = strings.TrimSpace(self.Pending + " " + decision.Action.Text)
							}
						}
						file.Close()
						if err = writeJSON(filepath.Join(dir, "admitted-evidence.json"), admitted); err != nil {
							return err
						}
						if err = writeJSON(filepath.Join(dir, "result.json"), map[string]any{
							"errors": runFailures, "generated_actions": history, "audible_adaptation_score": nil,
							"score_unavailable_reason": "no playback; generated text is not audible task completion",
							"no_feedback":              control,
						}); err != nil {
							return err
						}
						fmt.Printf("%s/%s/%s repeat=%d actions=%d errors=%d\n", cell.ID, pair.ID, branch.ID, repeat, len(history), runFailures)
					}
				}
			}
		}
	}
	return writeJSON(filepath.Join(*out, "summary.json"), map[string]any{
		"execution_mode": "virtual-time-text-only", "requests": attempts, "failures": failures,
		"audible_adaptation_score": nil, "finished": time.Now().UTC(),
	})
}
