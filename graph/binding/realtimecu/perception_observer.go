package realtimecu

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"

	"github.com/bojieli/OpenRealtime/perception"
)

// PerceptionObserverConfig adapts the repository's provider-neutral audio and
// video observers to the Realtime-CU observer plug-in contract. Both observers
// deliberately share one registered name: the session adapter binds that
// identity into every committed observation while Source preserves which
// sensor produced it.
type PerceptionObserverConfig struct {
	Name  string
	Audio perception.Observer
	Video perception.Observer
}

// NewPerceptionObserver composes independently selected perception plug-ins.
// It does not select an ASR, vision model, credential, or presentation layer;
// those remain launcher-owned factories.
func NewPerceptionObserver(config PerceptionObserverConfig) (Observer, error) {
	if strings.TrimSpace(config.Name) == "" || config.Name != strings.TrimSpace(config.Name) ||
		strings.ContainsAny(config.Name, "\x00\r\n") {
		return nil, errors.New("Realtime-CU perception observer requires a canonical name")
	}
	if nilPerceptionObserver(config.Audio) || nilPerceptionObserver(config.Video) {
		return nil, errors.New("Realtime-CU perception observer requires audio and video plug-ins")
	}
	if config.Audio.Name() != config.Name || config.Video.Name() != config.Name {
		return nil, errors.New("Realtime-CU perception observer plug-ins must share the registered name")
	}
	audioMu, videoMu := &sync.Mutex{}, &sync.Mutex{}
	if samePerceptionObserver(config.Audio, config.Video) {
		videoMu = audioMu
	}
	observer := &perceptionObserver{
		name: config.Name, audio: config.Audio, video: config.Video,
		audioMu: audioMu, videoMu: videoMu, closeDone: make(chan struct{}),
	}
	observer.stateCond = sync.NewCond(&observer.stateMu)
	return observer, nil
}

type perceptionObserver struct {
	name    string
	audio   perception.Observer
	video   perception.Observer
	audioMu *sync.Mutex
	videoMu *sync.Mutex

	stateMu   sync.Mutex
	stateCond *sync.Cond
	active    int
	closed    bool
	closeDone chan struct{}
}

func (observer *perceptionObserver) Audio(
	ctx context.Context, frame perception.Frame,
) ([]perception.Observation, error) {
	return observer.observe(ctx, observer.audioMu, observer.audio, frame, SourceMicrophone)
}

func (observer *perceptionObserver) Video(
	ctx context.Context, frame perception.Frame,
) ([]perception.Observation, error) {
	if frame.Source != SourceScreen && frame.Source != SourceCamera {
		return nil, fmt.Errorf("Realtime-CU perception observer rejects video source %q", frame.Source)
	}
	return observer.observe(ctx, observer.videoMu, observer.video, frame, frame.Source)
}

func (observer *perceptionObserver) observe(
	ctx context.Context, sensorMu *sync.Mutex, selected perception.Observer,
	frame perception.Frame, source string,
) ([]perception.Observation, error) {
	if ctx == nil {
		return nil, errors.New("observe Realtime-CU perception frame: nil context")
	}
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	sensorMu.Lock()
	defer sensorMu.Unlock()
	if !observer.begin() {
		return nil, errors.New("Realtime-CU perception observer is closed")
	}
	defer observer.end()
	if frame.Source != source || !selected.Accepts(frame) {
		return nil, fmt.Errorf("Realtime-CU perception plug-in does not accept %q", frame.Source)
	}
	if !selected.Gate(frame) {
		return nil, nil
	}
	observations, err := selected.Observe(ctx, []perception.Frame{frame})
	if err != nil {
		return nil, err
	}
	return observations, nil
}

func (observer *perceptionObserver) Consequence(
	ctx context.Context, consequence VisualConsequence,
) error {
	if ctx == nil {
		return errors.New("refresh Realtime-CU perception observer: nil context")
	}
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if consequence.TargetSource != SourceScreen {
		return fmt.Errorf("Realtime-CU visual consequence targets %q, want screen", consequence.TargetSource)
	}
	observer.videoMu.Lock()
	defer observer.videoMu.Unlock()
	if !observer.begin() {
		return errors.New("Realtime-CU perception observer is closed")
	}
	defer observer.end()
	if refreshable, ok := observer.video.(perception.SourceRefreshableObserver); ok {
		refreshable.RefreshSource(consequence.TargetSource)
		return nil
	}
	if refreshable, ok := observer.video.(perception.RefreshableObserver); ok {
		refreshable.RefreshNext()
		return nil
	}
	return errors.New("Realtime-CU video observer cannot refresh after an action")
}

func (observer *perceptionObserver) Close() error {
	observer.stateMu.Lock()
	if observer.closed {
		done := observer.closeDone
		observer.stateMu.Unlock()
		<-done
		return nil
	}
	observer.closed = true
	for observer.active != 0 {
		observer.stateCond.Wait()
	}
	audio, video := observer.audio, observer.video
	observer.stateMu.Unlock()

	var closeErr error
	if closer, ok := audio.(io.Closer); ok {
		closeErr = errors.Join(closeErr, closer.Close())
	} else {
		audio.Reset()
	}
	if !samePerceptionObserver(audio, video) {
		if closer, ok := video.(io.Closer); ok {
			closeErr = errors.Join(closeErr, closer.Close())
		} else {
			video.Reset()
		}
	}
	observer.stateMu.Lock()
	close(observer.closeDone)
	observer.stateMu.Unlock()
	return closeErr
}

func (observer *perceptionObserver) begin() bool {
	observer.stateMu.Lock()
	defer observer.stateMu.Unlock()
	if observer.closed {
		return false
	}
	observer.active++
	return true
}

func (observer *perceptionObserver) end() {
	observer.stateMu.Lock()
	observer.active--
	if observer.active == 0 {
		observer.stateCond.Broadcast()
	}
	observer.stateMu.Unlock()
}

func nilPerceptionObserver(observer perception.Observer) bool {
	if observer == nil {
		return true
	}
	value := reflect.ValueOf(observer)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func samePerceptionObserver(left, right perception.Observer) bool {
	leftValue, rightValue := reflect.ValueOf(left), reflect.ValueOf(right)
	return leftValue.IsValid() && rightValue.IsValid() && leftValue.Type() == rightValue.Type() &&
		leftValue.Type().Comparable() && leftValue.Interface() == rightValue.Interface()
}

var _ Observer = (*perceptionObserver)(nil)
