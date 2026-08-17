package reference

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

const demonstrationSchemaVersion = "0.1.0"

type TranslationSegment struct {
	EndMS            uint64 `json:"end_ms"`
	SourceDelta      string `json:"source_delta"`
	TargetDelta      string `json:"target_delta"`
	EarlyTargetDelta string `json:"early_target_delta"`
}

type TranslationDemonstration struct {
	SourceLanguage string               `json:"source_language"`
	TargetLanguage string               `json:"target_language"`
	Segments       []TranslationSegment `json:"segments"`
}

type GameRound struct {
	ID             string `json:"id"`
	CueAtMS        uint64 `json:"cue_at_ms"`
	DeadlineMS     uint64 `json:"deadline_ms"`
	Prompt         string `json:"prompt"`
	ExpectedAction string `json:"expected_action"`
}

type GameDemonstration struct {
	Name   string      `json:"name"`
	Rounds []GameRound `json:"rounds"`
}

type Demonstrations struct {
	SchemaVersion  string                   `json:"schema_version"`
	AnnotationMode string                   `json:"annotation_mode"`
	FixtureSHA256  string                   `json:"fixture_sha256"`
	Translation    TranslationDemonstration `json:"translation"`
	Game           GameDemonstration        `json:"game"`
}

// LoadDemonstrations loads the key-free, project-authored M5 evaluation adapter.
func LoadDemonstrations(path string) (Demonstrations, error) {
	file, err := os.Open(path)
	if err != nil {
		return Demonstrations{}, fmt.Errorf("read demonstration manifest: %w", err)
	}
	defer file.Close()
	const maximumBytes = int64(1 << 20)
	metadata, err := file.Stat()
	if err != nil {
		return Demonstrations{}, fmt.Errorf("stat demonstration manifest: %w", err)
	}
	if metadata.Size() > maximumBytes {
		return Demonstrations{}, errors.New("demonstration manifest exceeds 1 MiB")
	}
	var demonstrations Demonstrations
	decoder := json.NewDecoder(io.LimitReader(file, maximumBytes+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&demonstrations); err != nil {
		return Demonstrations{}, fmt.Errorf("decode demonstration manifest: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Demonstrations{}, errors.New("demonstration manifest must contain exactly one JSON value")
		}
		return Demonstrations{}, fmt.Errorf("decode trailing demonstration data: %w", err)
	}
	if err := demonstrations.validate(); err != nil {
		return Demonstrations{}, err
	}
	return demonstrations, nil
}

func (demonstrations Demonstrations) validate() error {
	if demonstrations.SchemaVersion != demonstrationSchemaVersion {
		return fmt.Errorf("unsupported demonstration schema %q", demonstrations.SchemaVersion)
	}
	if demonstrations.AnnotationMode != "symbolic_non_translation_and_game" {
		return fmt.Errorf("unsupported demonstration annotation mode %q", demonstrations.AnnotationMode)
	}
	digest, err := hex.DecodeString(demonstrations.FixtureSHA256)
	if err != nil || len(digest) != 32 {
		return errors.New("demonstration fixture_sha256 must be a 64-character hexadecimal digest")
	}
	translation := demonstrations.Translation
	if strings.TrimSpace(translation.SourceLanguage) == "" || strings.TrimSpace(translation.TargetLanguage) == "" {
		return errors.New("translation languages must not be empty")
	}
	if len(translation.Segments) == 0 {
		return errors.New("translation demonstration requires segments")
	}
	for index, segment := range translation.Segments {
		if segment.EndMS == 0 || segment.SourceDelta == "" || segment.TargetDelta == "" || segment.EarlyTargetDelta == "" {
			return fmt.Errorf("translation segment %d is incomplete", index)
		}
		if segment.EndMS%200 != 0 {
			return fmt.Errorf("translation segment %d must end on a 200 ms engine frame", index)
		}
		if index > 0 && segment.EndMS <= translation.Segments[index-1].EndMS {
			return errors.New("translation segments must be strictly ordered")
		}
	}
	if demonstrations.Game.Name == "" || len(demonstrations.Game.Rounds) == 0 {
		return errors.New("game demonstration requires a name and rounds")
	}
	seen := make(map[string]struct{}, len(demonstrations.Game.Rounds))
	for index, round := range demonstrations.Game.Rounds {
		if round.ID == "" || round.DeadlineMS == 0 || round.Prompt == "" || round.ExpectedAction == "" {
			return fmt.Errorf("game round %d is incomplete", index)
		}
		if _, exists := seen[round.ID]; exists {
			return fmt.Errorf("duplicate game round %q", round.ID)
		}
		seen[round.ID] = struct{}{}
		if index > 0 && round.CueAtMS <= demonstrations.Game.Rounds[index-1].CueAtMS {
			return errors.New("game rounds must be strictly ordered by cue time")
		}
	}
	return nil
}
