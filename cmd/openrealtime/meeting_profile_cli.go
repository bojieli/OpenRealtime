package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	launchprofile "github.com/bojieli/OpenRealtime/graph/launch/profile"
	"github.com/bojieli/OpenRealtime/internal/fileidentity"
)

type meetingProfileCommandDependencies struct {
	executable func() (inspect.ArtifactIdentity, error)
	verifier   func() (meetingDeploymentVerifier, error)
}

func runMeetingProfileFreeze(arguments []string, output io.Writer) error {
	return runMeetingProfileFreezeWithDependencies(arguments, output, meetingProfileCommandDependencies{
		executable: func() (inspect.ArtifactIdentity, error) {
			artifacts, err := executableServeProfileArtifacts()
			return artifacts.Gateway, err
		},
		verifier: newLocalMeetingDeploymentVerifier,
	})
}

func runMeetingProfileFreezeWithDependencies(
	arguments []string, output io.Writer, dependencies meetingProfileCommandDependencies,
) error {
	if output == nil || dependencies.executable == nil || dependencies.verifier == nil {
		return errors.New("profile meeting requires complete command dependencies")
	}
	options := defaultMeetingProfileOptions()
	flags := flag.NewFlagSet("openrealtime profile meeting", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.StringVar(&options.out, "out", "", "new absolute Meeting launch-profile YAML path")
	flags.StringVar(&options.graphOut, "graph-out", "", "new absolute exact bound Graph IR JSON path")
	flags.StringVar(&options.valuesOut, "values-out", "", "new absolute exact element-values JSON path")
	flags.StringVar(&options.resolutionOut, "resolution-out", "", "new absolute expected live-resolution JSON path")
	flags.StringVar(&options.executionOut, "execution-out", "", "new absolute reviewed execution-requirement JSON path")
	flags.StringVar(&options.name, "name", options.name, "immutable profile name")
	flags.Uint64Var(&options.revision, "revision", options.revision, "positive profile revision")
	flags.StringVar(&options.tokenEnv, "token-env", options.tokenEnv, "required gateway bearer-token environment name")
	flags.StringVar(&options.operatorCapabilityEnv, "operator-capability-env", options.operatorCapabilityEnv,
		"optional separate mgmt_ operator-capability environment name")
	flags.Uint64Var(&options.inspectionTTL, "inspection-token-ttl-ms", options.inspectionTTL, "runtime-inspection token lifetime")
	flags.IntVar(&options.maxAudioBytes, "max-audio-frame-bytes", options.maxAudioBytes, "Realtime audio-frame bound")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("profile meeting accepts flags only")
	}
	verifier, err := dependencies.verifier()
	if err != nil {
		return fmt.Errorf("construct live Meeting deployment verifier: %w", err)
	}
	if nilMeetingDeploymentInterface(verifier) {
		return errors.New("construct live Meeting deployment verifier: nil verifier")
	}
	options.verifier = verifier
	options.deployments, err = verifier.Resolve(context.Background())
	if err != nil {
		return fmt.Errorf("resolve live Meeting deployments for profile freeze: %w", err)
	}
	if err := validateMeetingProfileOptions(options); err != nil {
		return err
	}
	executable, err := dependencies.executable()
	if err != nil {
		return err
	}
	frozen, err := freezeProductionMeetingProfile(context.Background(), options, executable)
	if err != nil {
		return err
	}
	return writeFrozenMeetingProfile(output, options, frozen)
}

func validateMeetingProfileOptions(options meetingProfileOptions) error {
	paths := []string{
		options.out, options.graphOut, options.valuesOut, options.resolutionOut, options.executionOut,
	}
	seen := make(map[string]struct{}, len(paths))
	campaignDirectory := ""
	for _, path := range paths {
		if strings.TrimSpace(path) == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return errors.New("profile meeting requires all five absolute canonical output paths")
		}
		if _, duplicate := seen[path]; duplicate {
			return errors.New("profile meeting output paths must be distinct")
		}
		seen[path] = struct{}{}
		if campaignDirectory == "" {
			campaignDirectory = filepath.Dir(path)
		} else if filepath.Dir(path) != campaignDirectory {
			return errors.New("profile meeting outputs must share one create-only campaign directory")
		}
	}
	if campaignDirectory == filepath.Dir(campaignDirectory) {
		return errors.New("profile meeting campaign directory cannot be a filesystem root")
	}
	if strings.TrimSpace(options.name) == "" || options.revision == 0 ||
		strings.TrimSpace(options.tokenEnv) == "" || strings.TrimSpace(options.tokenEnv) != options.tokenEnv ||
		strings.TrimSpace(options.operatorCapabilityEnv) != options.operatorCapabilityEnv ||
		options.inspectionTTL == 0 || options.maxAudioBytes <= 0 {
		return errors.New("profile meeting has invalid identity or server bounds")
	}
	if nilMeetingDeploymentInterface(options.verifier) {
		return errors.New("profile meeting requires a live deployment verifier")
	}
	return options.deployments.validate()
}

type meetingProfileArtifactPublication struct {
	label   string
	path    string
	payload []byte
}

func writeFrozenMeetingProfile(
	output io.Writer, options meetingProfileOptions, frozen frozenMeetingProfile,
) error {
	profilePayload, err := launchprofile.MarshalYAML(frozen.Profile)
	if err != nil {
		return err
	}
	graphPayload, err := frozen.Plan.Graph().Marshal()
	if err != nil {
		return err
	}
	valuesPayload, err := json.MarshalIndent(frozen.Values, "", "  ")
	if err != nil {
		return err
	}
	valuesPayload = append(valuesPayload, '\n')
	resolutionPayload, err := bench.MarshalExpectedResolution(frozen.Resolution)
	if err != nil {
		return err
	}
	executionPayload, err := bench.MarshalExecutionRequirement(frozen.Execution)
	if err != nil {
		return err
	}
	artifacts := []meetingProfileArtifactPublication{
		{label: "bound Graph IR", path: options.graphOut, payload: graphPayload},
		{label: "element values", path: options.valuesOut, payload: valuesPayload},
		{label: "expected live resolution", path: options.resolutionOut, payload: resolutionPayload},
		{label: "reviewed execution requirement", path: options.executionOut, payload: executionPayload},
		{label: "launch profile", path: options.out, payload: profilePayload},
	}
	if err := publishMeetingProfileCampaign(options, artifacts, nil); err != nil {
		return err
	}
	fmt.Fprintf(output, "wrote %s\n", options.out)
	fmt.Fprintf(output, "graph       %s\n", options.graphOut)
	fmt.Fprintf(output, "values      %s\n", options.valuesOut)
	fmt.Fprintf(output, "resolution  %s\n", options.resolutionOut)
	fmt.Fprintf(output, "execution   %s\n", options.executionOut)
	fmt.Fprintf(output, "profile     %s@%d\n", frozen.Profile.Name, frozen.Profile.Revision)
	fmt.Fprintf(output, "fingerprint %s\n", frozen.Profile.Fingerprint)
	fmt.Fprintf(output, "plan        %s\n", frozen.Profile.Plan.PlanFingerprint)
	fmt.Fprintf(output, "providers   model=%s/%s asr=%s/%s tts=%s/%s background=%s/%s\n",
		meetingLocalModelProvider, meetingLocalModelName,
		meetingLocalASRProvider, meetingLocalASRModel,
		meetingLocalTTSProvider, meetingLocalTTSModel,
		meetingBackgroundProvider, meetingBackgroundModel)
	return nil
}

type meetingProfilePublicationHook func(completedFiles int) error

func publishMeetingProfileCampaign(
	options meetingProfileOptions, artifacts []meetingProfileArtifactPublication,
	hook meetingProfilePublicationHook,
) (resultErr error) {
	campaignDirectory := filepath.Dir(options.out)
	parentPath, campaignName := filepath.Dir(campaignDirectory), filepath.Base(campaignDirectory)
	if err := validateMeetingProfileParent(parentPath); err != nil {
		return errors.New("Meeting profile campaign parent is invalid")
	}
	parentBefore, err := os.Lstat(parentPath)
	if err != nil || parentBefore.Mode()&os.ModeSymlink != 0 || !parentBefore.IsDir() {
		return errors.New("Meeting profile campaign parent is invalid")
	}
	parent, err := os.OpenRoot(parentPath)
	if err != nil {
		return errors.New("open Meeting profile campaign parent")
	}
	defer func() {
		if closeErr := parent.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr, errors.New("close Meeting profile campaign parent"))
		}
	}()
	openedParent, openErr := parent.Stat(".")
	visibleParent, visibleErr := os.Lstat(parentPath)
	if openErr != nil || visibleErr != nil || visibleParent.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(parentBefore, openedParent) || !os.SameFile(openedParent, visibleParent) {
		return errors.New("Meeting profile campaign parent changed while opening")
	}
	if existing, err := parent.Lstat(campaignName); err == nil {
		if !existing.IsDir() || existing.Mode()&os.ModeSymlink != 0 {
			return errors.New("Meeting profile campaign path is not a directory")
		}
		if err := verifyMeetingProfileCampaign(parent, campaignName, artifacts); err != nil {
			return err
		}
		return syncMeetingProfileRoot(parent)
	} else if !os.IsNotExist(err) {
		return errors.New("inspect Meeting profile campaign path")
	}
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return errors.New("create Meeting profile campaign stage identity")
	}
	stageName := "." + campaignName + ".quarantine-" + hex.EncodeToString(entropy[:])
	if err := parent.Mkdir(stageName, 0o700); err != nil {
		return errors.New("create Meeting profile campaign stage")
	}
	published := false
	defer func() {
		if !published {
			if err := parent.RemoveAll(stageName); err != nil {
				resultErr = errors.Join(resultErr, errors.New("remove failed Meeting profile campaign stage"))
			}
		}
	}()
	stage, err := parent.OpenRoot(stageName)
	if err != nil {
		return errors.New("open Meeting profile campaign stage")
	}
	stageClosed := false
	defer func() {
		if !stageClosed {
			if closeErr := stage.Close(); closeErr != nil {
				resultErr = errors.Join(resultErr, errors.New("close Meeting profile campaign stage"))
			}
		}
	}()
	for index, artifact := range artifacts {
		name := filepath.Base(artifact.path)
		if filepath.Dir(artifact.path) != campaignDirectory || name == "." || name == "" {
			return errors.New("Meeting profile artifact escaped its campaign directory")
		}
		file, err := stage.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return fmt.Errorf("write Meeting %s: create artifact", artifact.label)
		}
		_, writeErr := file.Write(artifact.payload)
		syncErr := file.Sync()
		closeErr := file.Close()
		if writeErr != nil || syncErr != nil || closeErr != nil {
			return fmt.Errorf("write Meeting %s: retain artifact", artifact.label)
		}
		if hook != nil {
			if err := hook(index + 1); err != nil {
				return err
			}
		}
	}
	if err := syncMeetingProfileRoot(stage); err != nil {
		return errors.New("sync Meeting profile campaign stage")
	}
	if err := stage.Close(); err != nil {
		return errors.New("close Meeting profile campaign stage before publication")
	}
	stageClosed = true
	if err := renameMeetingProfileNoReplace(
		parent, openedParent, stageName, campaignName,
	); err != nil {
		return errors.New("publish Meeting profile campaign atomically")
	}
	published = true
	if err := syncMeetingProfileRoot(parent); err != nil {
		return errors.New("sync published Meeting profile campaign parent")
	}
	entry, entryErr := parent.Lstat(campaignName)
	visible, visibleErr := os.Lstat(campaignDirectory)
	parentAfter, parentAfterErr := os.Lstat(parentPath)
	if entryErr != nil || visibleErr != nil || parentAfterErr != nil || !entry.IsDir() ||
		entry.Mode()&os.ModeSymlink != 0 || !os.SameFile(entry, visible) ||
		!os.SameFile(openedParent, parentAfter) {
		return errors.New("published Meeting profile campaign identity changed")
	}
	return verifyMeetingProfileCampaign(parent, campaignName, artifacts)
}

func syncMeetingProfileRoot(root *os.Root) error {
	file, err := root.Open(".")
	if err != nil {
		return err
	}
	syncErr := file.Sync()
	return errors.Join(syncErr, file.Close())
}

func verifyMeetingProfileCampaign(
	parent *os.Root, campaignName string, artifacts []meetingProfileArtifactPublication,
) (resultErr error) {
	entryBefore, err := parent.Lstat(campaignName)
	if err != nil || !entryBefore.IsDir() || entryBefore.Mode()&os.ModeSymlink != 0 {
		return errors.New("inspect existing Meeting profile campaign identity")
	}
	root, err := parent.OpenRoot(campaignName)
	if err != nil {
		return errors.New("open existing Meeting profile campaign")
	}
	defer func() {
		if closeErr := root.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr, errors.New("close existing Meeting profile campaign"))
		}
	}()
	openedRoot, openErr := root.Stat(".")
	if openErr != nil || !os.SameFile(entryBefore, openedRoot) {
		return errors.New("existing Meeting profile campaign changed while opening")
	}
	directory, err := root.Open(".")
	if err != nil {
		return errors.New("list existing Meeting profile campaign")
	}
	entries, readErr := directory.ReadDir(-1)
	closeErr := directory.Close()
	if readErr != nil || closeErr != nil || len(entries) != len(artifacts) {
		return errors.New("existing Meeting profile campaign has the wrong artifact set")
	}
	wanted := make(map[string][]byte, len(artifacts))
	for _, artifact := range artifacts {
		wanted[filepath.Base(artifact.path)] = artifact.payload
	}
	for _, entry := range entries {
		expected, ok := wanted[entry.Name()]
		if !ok || entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return errors.New("existing Meeting profile campaign has an unexpected artifact")
		}
		before, err := root.Lstat(entry.Name())
		if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() ||
			before.Size() != int64(len(expected)) {
			return errors.New("existing Meeting profile artifact has an invalid identity")
		}
		file, err := root.Open(entry.Name())
		if err != nil {
			return errors.New("open existing Meeting profile artifact")
		}
		opened, statErr := file.Stat()
		payload, readErr := io.ReadAll(io.LimitReader(file, int64(len(expected))+1))
		linkErr := fileidentity.RequireSingleLink(file)
		closeErr := file.Close()
		after, afterErr := root.Lstat(entry.Name())
		if statErr != nil || readErr != nil || linkErr != nil || closeErr != nil || afterErr != nil ||
			!os.SameFile(before, opened) || !os.SameFile(opened, after) ||
			after.Size() != int64(len(expected)) || !bytes.Equal(payload, expected) {
			return errors.New("existing Meeting profile artifact differs from the requested campaign")
		}
	}
	entryAfter, entryErr := parent.Lstat(campaignName)
	rootAfter, rootErr := root.Stat(".")
	if entryErr != nil || rootErr != nil || !os.SameFile(entryBefore, entryAfter) ||
		!os.SameFile(openedRoot, rootAfter) || !os.SameFile(entryAfter, rootAfter) {
		return errors.New("existing Meeting profile campaign changed during verification")
	}
	return nil
}

func validateMeetingProfileParent(path string) error {
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("Meeting profile parent is missing, symlinked, or not a directory")
		}
		if current == filepath.Dir(current) {
			return nil
		}
	}
}
