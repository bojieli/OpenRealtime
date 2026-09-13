package deepgram

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
)

// LanguageMux keeps two explicitly selected Deepgram language streams over
// the same utterance. Deepgram's streaming language detection is unavailable,
// and Nova-3's native multi mode does not include Mandarin. A deployment that
// knows it needs English and Mandarin therefore has to preserve both readings
// until the script itself distinguishes them.
//
// The first language is the ordinary/default lane. The second must be a
// Chinese locale. A lane wins only after Deepgram gives its current reading a
// high confidence; this keeps both an English phonetic rendering of Mandarin
// and a Chinese model's occasional Han-like rendering of English away from the
// transcript-event policy. Once selected, the lane remains fixed for the
// utterance.
type LanguageMux struct {
	mu sync.Mutex

	primary    languageStream
	chinese    languageStream
	descriptor v1.Descriptor

	selectedChinese bool
	selectedPrimary bool
	pendingPrimary  *v1.PerceptionRevision
	primaryLatest   v1.PerceptionRevision
	chineseLatest   v1.PerceptionRevision
	revisionID      uint64
	lastText        string
}

type languageStream interface {
	v1.PerceptionProvider
	SpeechEndpointed() bool
	Confidence() float64
	EndUtterance() error
	Close() error
}

const languageSelectionConfidence = 0.85

// NewLanguageMux creates an explicit English/Chinese streaming recogniser.
// Both lanes use the same Deepgram model and connection settings; only the
// service language restriction differs.
func NewLanguageMux(config ListenConfig, languages []string) (*LanguageMux, error) {
	if len(languages) != 2 {
		return nil, errors.New("Deepgram language multiplexing requires exactly two languages")
	}
	primaryLanguage := strings.TrimSpace(languages[0])
	chineseLanguage := strings.TrimSpace(languages[1])
	if primaryLanguage == "" || chineseLanguage == "" {
		return nil, errors.New("Deepgram language multiplexing requires two non-empty languages")
	}
	if !strings.HasPrefix(strings.ToLower(chineseLanguage), "zh") {
		return nil, fmt.Errorf("Deepgram secondary multiplexed language %q is not a Chinese locale", chineseLanguage)
	}
	primaryConfig := config
	primaryConfig.Language = primaryLanguage
	primary, err := NewListener(primaryConfig)
	if err != nil {
		return nil, err
	}
	chineseConfig := config
	chineseConfig.Language = chineseLanguage
	chinese, err := NewListener(chineseConfig)
	if err != nil {
		_ = primary.Close()
		return nil, err
	}
	descriptor := primary.Descriptor()
	descriptor.Version = "deepgram-listen-language-mux-2"
	if config.Persistent {
		descriptor.Version = "deepgram-listen-language-mux-persistent-1"
	}
	return newLanguageMux(primary, chinese, descriptor), nil
}

func newLanguageMux(primary, chinese languageStream, descriptor v1.Descriptor) *LanguageMux {
	return &LanguageMux{primary: primary, chinese: chinese, descriptor: descriptor}
}

func (mux *LanguageMux) Descriptor() v1.Descriptor {
	mux.mu.Lock()
	defer mux.mu.Unlock()
	descriptor := mux.descriptor
	descriptor.Capabilities = cloneCapabilities(descriptor.Capabilities)
	return descriptor
}

func cloneCapabilities(source v1.Capabilities) v1.Capabilities {
	result := make(v1.Capabilities, len(source))
	for capability, enabled := range source {
		result[capability] = enabled
	}
	return result
}

func (mux *LanguageMux) PushFrame(
	ctx context.Context, frame v1.AudioFrame,
) ([]v1.PerceptionRevision, error) {
	mux.mu.Lock()
	defer mux.mu.Unlock()
	primary, chinese, err := pushBoth(ctx, mux.primary, mux.chinese, frame)
	if err != nil {
		return nil, err
	}
	primaryChanged := len(primary) > 0
	chineseChanged := len(chinese) > 0
	if primaryChanged {
		mux.primaryLatest = primary[len(primary)-1]
	}
	if chineseChanged {
		mux.chineseLatest = chinese[len(chinese)-1]
	}
	// Short greetings and loanwords (for example 哈喽 for "hello") are
	// not enough evidence to lock out the rest of an English utterance.
	// An English selection is not final either: the English lane crossed
	// the bar once on "Nihal." and the whole Mandarin greeting that
	// followed, read by the Chinese lane at 0.99, was never let through.
	if !mux.selectedChinese && hanCount(revisionText(mux.chineseLatest)) >= 4 &&
		mux.chinese.Confidence() >= languageSelectionConfidence &&
		(revisionText(mux.primaryLatest) == "" || mux.chinese.Confidence() > mux.primary.Confidence()) {
		mux.selectedChinese = true
		mux.selectedPrimary = false
		mux.pendingPrimary = nil
	}
	if mux.selectedChinese {
		if !chineseChanged {
			return nil, nil
		}
		return mux.emit(mux.chineseLatest, false), nil
	}
	if primaryChanged {
		latest := mux.primaryLatest
		mux.pendingPrimary = &latest
	}
	if !mux.selectedPrimary && mux.pendingPrimary != nil &&
		mux.primary.Confidence() >= languageSelectionConfidence {
		mux.selectedPrimary = true
	}
	if !mux.selectedPrimary || !primaryChanged {
		return nil, nil
	}
	ready := *mux.pendingPrimary
	mux.pendingPrimary = nil
	return mux.emit(ready, false), nil
}

func pushBoth(
	ctx context.Context, primary, chinese languageStream, frame v1.AudioFrame,
) ([]v1.PerceptionRevision, []v1.PerceptionRevision, error) {
	type outcome struct {
		chinese   bool
		revisions []v1.PerceptionRevision
		err       error
	}
	results := make(chan outcome, 2)
	go func() {
		revisions, err := primary.PushFrame(ctx, frame)
		results <- outcome{revisions: revisions, err: err}
	}()
	go func() {
		revisions, err := chinese.PushFrame(ctx, frame)
		results <- outcome{chinese: true, revisions: revisions, err: err}
	}()
	var primaryRevisions, chineseRevisions []v1.PerceptionRevision
	for range 2 {
		result := <-results
		if result.err != nil {
			return nil, nil, result.err
		}
		if result.chinese {
			chineseRevisions = result.revisions
		} else {
			primaryRevisions = result.revisions
		}
	}
	return primaryRevisions, chineseRevisions, nil
}

func (mux *LanguageMux) Finalize(
	ctx context.Context, sourceSample uint64,
) (v1.PerceptionRevision, error) {
	mux.mu.Lock()
	defer mux.mu.Unlock()
	type outcome struct {
		chinese  bool
		revision v1.PerceptionRevision
		err      error
	}
	results := make(chan outcome, 2)
	go func() {
		revision, err := mux.primary.Finalize(ctx, sourceSample)
		results <- outcome{revision: revision, err: err}
	}()
	go func() {
		revision, err := mux.chinese.Finalize(ctx, sourceSample)
		results <- outcome{chinese: true, revision: revision, err: err}
	}()
	var primary, chinese v1.PerceptionRevision
	for range 2 {
		result := <-results
		if result.err != nil {
			return v1.PerceptionRevision{}, result.err
		}
		if result.chinese {
			chinese = result.revision
		} else {
			primary = result.revision
		}
	}
	chosen := primary
	// The Chinese lane wins the endpoint only when it is as sure as the
	// mid-utterance rule demands: a one-second fragment of accented English
	// came back from it as "圣骑士", beating an English lane that was surer
	// of nothing.
	// Compared after both lanes have finalised: a lane's final carries the
	// reading it settled on, and its confidence with it. Read before, the
	// Chinese lane's 0.83 mid-sentence lost to English although its final
	// read the whole greeting at 0.99.
	if mux.selectedChinese || (carriesHan(revisionText(chinese)) &&
		mux.chinese.Confidence() >= languageSelectionConfidence &&
		mux.chinese.Confidence() > mux.primary.Confidence()) {
		chosen = chinese
	}
	chosen.SourceSample = sourceSample
	revision := mux.makeRevision(chosen, true)
	mux.resetUtterance()
	return revision, nil
}

// EndUtterance implements api/v1.UtteranceReusable for both lanes. The lane
// selection is per utterance - the next sentence may be in the other
// language - so it is forgotten with everything else.
func (mux *LanguageMux) EndUtterance() error {
	mux.mu.Lock()
	defer mux.mu.Unlock()
	primaryErr := mux.primary.EndUtterance()
	chineseErr := mux.chinese.EndUtterance()
	mux.resetUtterance()
	return errors.Join(primaryErr, chineseErr)
}

func (mux *LanguageMux) resetUtterance() {
	mux.selectedChinese, mux.selectedPrimary = false, false
	mux.pendingPrimary = nil
	mux.primaryLatest, mux.chineseLatest = v1.PerceptionRevision{}, v1.PerceptionRevision{}
	mux.lastText = ""
}

func (mux *LanguageMux) SpeechEndpointed() bool {
	mux.mu.Lock()
	defer mux.mu.Unlock()
	if mux.selectedChinese {
		return mux.chinese.SpeechEndpointed()
	}
	if mux.selectedPrimary {
		return mux.primary.SpeechEndpointed()
	}
	// Until script selects a lane, do not let an English phonetic hypothesis
	// close a Mandarin utterance before the Chinese stream catches up.
	return mux.primary.SpeechEndpointed() && mux.chinese.SpeechEndpointed()
}

func (mux *LanguageMux) Close() error {
	mux.mu.Lock()
	defer mux.mu.Unlock()
	primaryErr := mux.primary.Close()
	chineseErr := mux.chinese.Close()
	return errors.Join(primaryErr, chineseErr)
}

func (mux *LanguageMux) emit(source v1.PerceptionRevision, final bool) []v1.PerceptionRevision {
	if !final && revisionText(source) == mux.lastText {
		return nil
	}
	revision := mux.makeRevision(source, final)
	return []v1.PerceptionRevision{revision}
}

func (mux *LanguageMux) makeRevision(source v1.PerceptionRevision, final bool) v1.PerceptionRevision {
	text := revisionText(source)
	mux.revisionID++
	revision := v1.PerceptionRevision{
		RevisionID: mux.revisionID, SourceSample: source.SourceSample,
		StableText: source.StableText, UnstableText: source.UnstableText,
		Delta: textDelta(mux.lastText, text), Final: final,
	}
	if final {
		revision.StableText = text
		revision.UnstableText = ""
	}
	mux.lastText = text
	return revision
}

func revisionText(revision v1.PerceptionRevision) string {
	return strings.TrimSpace(revision.StableText + revision.UnstableText)
}

func carriesHan(text string) bool { return hanCount(text) > 0 }

func hanCount(text string) int {
	count := 0
	for _, symbol := range text {
		if unicode.Is(unicode.Han, symbol) {
			count++
		}
	}
	return count
}
