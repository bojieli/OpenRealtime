package media

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
)

func TestNewOwnsAndClosesSuccessfulClaimsBeforeObservingCancellation(t *testing.T) {
	for _, boundary := range []string{"encoder", "attestor"} {
		t.Run(boundary, func(t *testing.T) {
			for iteration := 0; iteration < 32; iteration++ {
				directory := filepath.Join(t.TempDir(), "attempt")
				ctx, cancel := context.WithCancel(t.Context())
				encoder, attestor := newFixtureEncoder(""), newFixtureAttestor("")
				if iteration == 0 {
					if boundary == "encoder" {
						encoder.mode = "close_error"
					} else {
						attestor.mode = "close_error"
					}
				}
				cancelFromConcurrentGoroutine := func() {
					done := make(chan struct{})
					go func() {
						cancel()
						close(done)
					}()
					<-done
				}
				if boundary == "encoder" {
					encoder.claimHook = cancelFromConcurrentGoroutine
				} else {
					attestor.claimHook = cancelFromConcurrentGoroutine
				}
				recorder, err := New(ctx, Config{
					Directory: directory, RequireAudio: true,
					ExpectedVideoSources: []string{"screen"}, Encoder: encoder, Attestor: attestor,
				})
				if recorder != nil || !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "super-secret") {
					t.Fatalf("iteration %d: New() = %v, %v", iteration, recorder, err)
				}
				wantEncoderCloses, wantAttestorCloses := int32(1), int32(0)
				if boundary == "attestor" {
					wantAttestorCloses = 1
				}
				if got := encoder.closeCalls.Load(); got != wantEncoderCloses {
					t.Fatalf("iteration %d: encoder Close calls = %d, want %d", iteration, got, wantEncoderCloses)
				}
				if got := attestor.closeCalls.Load(); got != wantAttestorCloses {
					t.Fatalf("iteration %d: attestor Close calls = %d, want %d", iteration, got, wantAttestorCloses)
				}
				if _, statErr := os.Lstat(directory); !os.IsNotExist(statErr) {
					t.Fatalf("iteration %d: canceled constructor consumed exact path: %v", iteration, statErr)
				}
				retry := mustRecorder(t, directory, []string{"screen"},
					newFixtureEncoder(""), newFixtureAttestor(""), nil)
				if err := retry.Abort(); err != nil {
					t.Fatalf("iteration %d: path/ownership retry = %v", iteration, err)
				}
			}
		})
	}
}

func TestConcurrentConstructorsDoNotDoubleOwnOrClosePlugIns(t *testing.T) {
	encoder, attestor := newFixtureEncoder(""), newFixtureAttestor("")
	type result struct {
		recorder *Recorder
		err      error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	directories := []string{
		filepath.Join(t.TempDir(), "attempt-one"),
		filepath.Join(t.TempDir(), "attempt-two"),
	}
	for _, directory := range directories {
		directory := directory
		go func() {
			<-start
			recorder, err := New(t.Context(), Config{
				Directory: directory, RequireAudio: true, ExpectedVideoSources: []string{"screen"},
				Encoder: encoder, Attestor: attestor,
			})
			results <- result{recorder: recorder, err: err}
		}()
	}
	close(start)
	first, second := <-results, <-results
	winners := []result{}
	losers := []result{}
	for _, got := range []result{first, second} {
		if got.err == nil {
			winners = append(winners, got)
		} else {
			losers = append(losers, got)
		}
	}
	if len(winners) != 1 || winners[0].recorder == nil || len(losers) != 1 || losers[0].recorder != nil {
		t.Fatalf("concurrent New results = first(%v,%v), second(%v,%v)",
			first.recorder, first.err, second.recorder, second.err)
	}
	if encoder.closeCalls.Load() != 0 || attestor.closeCalls.Load() != 0 {
		t.Fatalf("losing constructor closed winner's ownership: encoder=%d attestor=%d",
			encoder.closeCalls.Load(), attestor.closeCalls.Load())
	}
	if err := winners[0].recorder.Abort(); err != nil {
		t.Fatal(err)
	}
	if encoder.closeCalls.Load() != 1 || attestor.closeCalls.Load() != 1 {
		t.Fatalf("winning owner Close calls = encoder:%d attestor:%d",
			encoder.closeCalls.Load(), attestor.closeCalls.Load())
	}
}

func TestSensitiveValuesAreBoundedBeforeDirectoryOrVariantWork(t *testing.T) {
	valuesByCase := map[string][]string{
		"count": make([]string, maximumSensitiveValues+1),
		"value": {strings.Repeat("v", maximumSensitiveValueBytes+1)},
	}
	for index := range valuesByCase["count"] {
		valuesByCase["count"][index] = fmt.Sprintf("secret-%03d", index)
	}
	aggregate := make([]string, 0, maximumSensitiveBytes/maximumSensitiveValueBytes+1)
	for index := 0; index <= maximumSensitiveBytes/maximumSensitiveValueBytes; index++ {
		prefix := fmt.Sprintf("%04d", index)
		aggregate = append(aggregate, prefix+strings.Repeat("a", maximumSensitiveValueBytes-len(prefix)))
	}
	valuesByCase["aggregate"] = aggregate

	for name, values := range valuesByCase {
		t.Run(name, func(t *testing.T) {
			directory := filepath.Join(t.TempDir(), "attempt")
			recorder, err := New(t.Context(), Config{
				Directory: directory, RequireAudio: true, SensitiveValues: values,
			})
			if recorder != nil || err == nil || !strings.Contains(err.Error(), "bounded") &&
				!strings.Contains(err.Error(), "limit") {
				t.Fatalf("New() = %v, %v", recorder, err)
			}
			if _, statErr := os.Lstat(directory); !os.IsNotExist(statErr) {
				t.Fatalf("sensitive bound failure created attempt directory: %v", statErr)
			}
		})
	}

	accepted := make([]string, 0, maximumSensitiveBytes/maximumSensitiveValueBytes)
	for index := 0; index < maximumSensitiveBytes/maximumSensitiveValueBytes; index++ {
		prefix := fmt.Sprintf("%04d", index)
		accepted = append(accepted, prefix+strings.Repeat("b", maximumSensitiveValueBytes-len(prefix)))
	}
	directory := filepath.Join(t.TempDir(), "accepted")
	recorder, err := New(t.Context(), Config{
		Directory: directory, RequireAudio: true, SensitiveValues: accepted,
	})
	if err != nil {
		t.Fatalf("exact aggregate bound: %v", err)
	}
	if err := recorder.Abort(); err != nil {
		t.Fatal(err)
	}
	if _, err := newSensitiveMatcher(append(accepted,
		strings.Repeat("x", maximumSensitiveValueBytes))); err == nil {
		t.Fatal("sensitive matcher accepted work above its aggregate bound")
	}
}

func TestFinalizeReturnsVerifiedReceiptAlongsideObservableRootCloseFailure(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "attempt")
	t.Cleanup(func() { _ = makeTreeOwnerWritable(directory) })
	operations := defaultRecorderOperations()
	operations.closeRoot = func(root *os.Root) error {
		_ = root.Close()
		return errors.New("injected close detail")
	}
	recorder, err := newRecorder(t.Context(), Config{Directory: directory, RequireAudio: true}, operations)
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.CaptureAudio(testAudioCapture()); err != nil {
		t.Fatal(err)
	}
	receipt, err := recorder.Finalize(t.Context(), 500)
	if err == nil || !receipt.Manifest.Complete || !validDigest(receipt.ManifestSHA256) ||
		strings.Contains(err.Error(), "injected") {
		t.Fatalf("Finalize() = %+v, %v", receipt, err)
	}
	if _, verifyErr := VerifyBundle(directory, receipt.ManifestSHA256); verifyErr != nil {
		t.Fatalf("receipt returned with cleanup error did not identify exact verified bundle: %v", verifyErr)
	}
	again, againErr := recorder.Finalize(t.Context(), 500)
	if !reflect.DeepEqual(again, receipt) || againErr == nil || againErr.Error() != err.Error() {
		t.Fatalf("repeated Finalize() = %+v, %v; want %+v, %v", again, againErr, receipt, err)
	}
}

func TestFinalizeVerificationFailureNeverOrphansNominalMarker(t *testing.T) {
	for _, fallbackFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("owned_removal_fails_%t", fallbackFails), func(t *testing.T) {
			directory := filepath.Join(t.TempDir(), "attempt")
			t.Cleanup(func() { _ = makeTreeOwnerWritable(directory) })
			operations := defaultRecorderOperations()
			operations.verifyBundle = func(string, string) (Manifest, error) {
				return Manifest{}, errors.New("injected verifier detail")
			}
			operations.invalidatePublishedManifestRoot = func(*os.Root) error {
				return errors.New("injected invalidation detail")
			}
			if fallbackFails {
				operations.removeOwnedAttempt = func(string, os.FileInfo) error {
					return errors.New("injected removal detail")
				}
			}
			recorder, err := newRecorder(t.Context(), Config{
				Directory: directory, RequireAudio: true,
			}, operations)
			if err != nil {
				t.Fatal(err)
			}
			if err := recorder.CaptureAudio(testAudioCapture()); err != nil {
				t.Fatal(err)
			}
			receipt, err := recorder.Finalize(t.Context(), 500)
			if err == nil || strings.Contains(err.Error(), "injected") {
				t.Fatalf("Finalize() = %+v, %v", receipt, err)
			}
			if fallbackFails {
				if !receipt.Manifest.Complete || !validDigest(receipt.ManifestSHA256) ||
					!strings.Contains(err.Error(), "residual published") {
					t.Fatalf("residual result = %+v, %v", receipt, err)
				}
				if _, statErr := os.Lstat(filepath.Join(directory, manifestPath)); statErr != nil {
					t.Fatalf("published marker was not retained by the injected refusal: %v", statErr)
				}
				again, againErr := recorder.Finalize(t.Context(), 500)
				if !reflect.DeepEqual(again, receipt) || againErr == nil || againErr.Error() != err.Error() {
					t.Fatalf("repeated residual result = %+v, %v", again, againErr)
				}
			} else {
				if receipt.Manifest.Complete || receipt.ManifestSHA256 != "" {
					t.Fatalf("removed failed bundle returned a completion receipt: %+v", receipt)
				}
				if _, statErr := os.Lstat(directory); !os.IsNotExist(statErr) {
					t.Fatalf("owned fallback did not remove failed bundle: %v", statErr)
				}
			}
		})
	}
}

func TestFinalizeIdentifiesResidualUnexpectedMarkerWhenFilesystemRefusesCleanup(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "attempt")
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	operations := defaultRecorderOperations()
	operations.removeUnexpectedManifest = func(*os.Root) error {
		return errors.New("injected marker cleanup detail")
	}
	operations.removeOwnedAttempt = func(string, os.FileInfo) error {
		return errors.New("injected owned cleanup detail")
	}
	recorder, err := newRecorder(t.Context(), Config{Directory: directory, RequireAudio: true}, operations)
	if err != nil {
		t.Fatal(err)
	}
	marker := []byte(`{"complete":true}`)
	if err := os.WriteFile(filepath.Join(directory, manifestPath), marker, 0o400); err != nil {
		t.Fatal(err)
	}
	receipt, err := recorder.Finalize(t.Context(), 500)
	if err == nil || receipt.Manifest.Complete || receipt.ManifestSHA256 != digest(marker) ||
		!strings.Contains(err.Error(), "residual attempt media completion marker") ||
		strings.Contains(err.Error(), "injected") {
		t.Fatalf("Finalize() = %+v, %v", receipt, err)
	}
	again, againErr := recorder.Finalize(t.Context(), 500)
	if !reflect.DeepEqual(again, receipt) || againErr == nil || againErr.Error() != err.Error() {
		t.Fatalf("repeated Finalize() = %+v, %v", again, againErr)
	}
}

func TestAbortSurfacesCleanupFailureAndRemovesResidualMarker(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "attempt")
	operations := defaultRecorderOperations()
	operations.removeUnexpectedManifest = func(*os.Root) error {
		return errors.New("injected marker removal detail")
	}
	recorder, err := newRecorder(t.Context(), Config{Directory: directory, RequireAudio: true}, operations)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, manifestPath), []byte(`{"complete":true}`), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Abort(); err == nil || strings.Contains(err.Error(), "injected") {
		t.Fatalf("Abort() = %v", err)
	}
	if _, statErr := os.Lstat(directory); !os.IsNotExist(statErr) {
		t.Fatalf("Abort left a nominal completion marker: %v", statErr)
	}
}

func TestAbortRootCloseFailureIsCanonicalAndIdempotent(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "attempt")
	operations := defaultRecorderOperations()
	operations.closeRoot = func(root *os.Root) error {
		_ = root.Close()
		return errors.New("injected close detail")
	}
	recorder, err := newRecorder(t.Context(), Config{Directory: directory, RequireAudio: true}, operations)
	if err != nil {
		t.Fatal(err)
	}
	first := recorder.Abort()
	if first == nil || !strings.Contains(first.Error(), "close attempt media directory during abort") ||
		strings.Contains(first.Error(), "injected") {
		t.Fatalf("Abort() = %v", first)
	}
	if second := recorder.Abort(); second == nil || second.Error() != first.Error() {
		t.Fatalf("repeated Abort() = %v, want %v", second, first)
	}
}

func TestConstructorCleanupFailuresAreCanonicalAndCloseClaimsOnce(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "attempt")
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	encoder, attestor := newFixtureEncoder(""), newFixtureAttestor("")
	interferenceDone := make(chan struct{})
	encoder.claimHook = func() {
		go func() {
			defer close(interferenceDone)
			for {
				if info, err := os.Lstat(directory); err == nil && info.IsDir() {
					_ = os.Mkdir(filepath.Join(directory, "frames"), 0o700)
					return
				}
			}
		}()
	}
	operations := defaultRecorderOperations()
	operations.closeRoot = func(root *os.Root) error {
		_ = root.Close()
		return errors.New("injected close detail")
	}
	operations.removeOwnedAttempt = func(string, os.FileInfo) error {
		return errors.New("injected removal detail")
	}
	recorder, err := newRecorder(t.Context(), Config{
		Directory: directory, RequireAudio: true, ExpectedVideoSources: []string{"screen"},
		Encoder: encoder, Attestor: attestor,
	}, operations)
	<-interferenceDone
	if recorder != nil || err == nil || strings.Contains(err.Error(), "injected") ||
		!strings.Contains(err.Error(), "close attempt media directory during constructor cleanup") ||
		!strings.Contains(err.Error(), "remove owned attempt media directory during constructor cleanup") {
		t.Fatalf("newRecorder() = %v, %v", recorder, err)
	}
	if encoder.closeCalls.Load() != 1 || attestor.closeCalls.Load() != 1 {
		t.Fatalf("constructor cleanup Close calls = encoder:%d attestor:%d",
			encoder.closeCalls.Load(), attestor.closeCalls.Load())
	}
}

func TestVerifyBundleRejectsConcurrentAcceptedArtifactMutation(t *testing.T) {
	for _, mutation := range []string{"in_place", "replacement"} {
		t.Run(mutation, func(t *testing.T) {
			directory := filepath.Join(t.TempDir(), "attempt")
			t.Cleanup(func() { _ = makeTreeOwnerWritable(directory) })
			recorder, err := New(t.Context(), Config{Directory: directory, RequireAudio: true})
			if err != nil {
				t.Fatal(err)
			}
			if err := recorder.CaptureAudio(bench.SessionAudioCapture{
				SampleRateHz: 24_000, RoomPCM16: make([]int16, 24_000),
			}); err != nil {
				t.Fatal(err)
			}
			receipt, err := recorder.Finalize(t.Context(), 1000)
			if err != nil {
				t.Fatal(err)
			}
			audioPath := filepath.Join(directory, "audio.stereo.wav")
			var payload []byte
			var mutationErr error
			_, err = verifyBundle(directory, receipt.ManifestSHA256, verifyBundleOptions{
				beforeFinalRevalidation: func() {
					done := make(chan struct{})
					go func() {
						defer close(done)
						payload, mutationErr = os.ReadFile(audioPath)
						if mutationErr != nil {
							return
						}
						if mutationErr = os.Chmod(directory, 0o700); mutationErr != nil {
							return
						}
						if mutation == "replacement" {
							if mutationErr = os.Remove(audioPath); mutationErr != nil {
								return
							}
						} else {
							if mutationErr = os.Chmod(audioPath, 0o600); mutationErr != nil {
								return
							}
							payload[len(payload)-1] ^= 1
						}
						mutationErr = os.WriteFile(audioPath, payload, 0o400)
					}()
					<-done
				},
			})
			if mutationErr != nil {
				t.Fatalf("mutate accepted artifact: %v", mutationErr)
			}
			if err == nil {
				t.Fatal("VerifyBundle accepted an artifact mutated after its initial validation")
			}
		})
	}
}

func TestVerifyBundleSurfacesRootCloseFailure(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "attempt")
	t.Cleanup(func() { _ = makeTreeOwnerWritable(directory) })
	recorder, err := New(t.Context(), Config{Directory: directory, RequireAudio: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.CaptureAudio(testAudioCapture()); err != nil {
		t.Fatal(err)
	}
	receipt, err := recorder.Finalize(t.Context(), 500)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := verifyBundle(directory, receipt.ManifestSHA256, verifyBundleOptions{
		closeRoot: func(root *os.Root) error {
			_ = root.Close()
			return errors.New("injected verifier close detail")
		},
	})
	if verified.Complete || err == nil || !strings.Contains(err.Error(), "close verified attempt media root") ||
		strings.Contains(err.Error(), "injected") {
		t.Fatalf("verifyBundle() = %+v, %v", verified, err)
	}
}
