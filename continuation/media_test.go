package continuation_test

import (
	"testing"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/trajectory"
)

func TestLatestMediaHandlesKeepsNewestObservationPerSource(t *testing.T) {
	t.Parallel()
	items := []trajectory.Item{
		mediaObservation("screen-old", "screen", "screen-old-a", "screen-old-b"),
		mediaObservation("camera-current", "camera", "camera-current"),
		mediaObservation("screen-current", "screen", "screen-current-a", "screen-current-b"),
	}

	selected := continuation.LatestMediaHandles(items)
	for _, handle := range []string{"camera-current", "screen-current-a", "screen-current-b"} {
		if _, ok := selected[handle]; !ok {
			t.Errorf("latest media handle %q was not selected", handle)
		}
	}
	for _, handle := range []string{"screen-old-a", "screen-old-b"} {
		if _, ok := selected[handle]; ok {
			t.Errorf("stale media handle %q was selected", handle)
		}
	}
}

func TestLatestMediaHandlesFallsBackToObserver(t *testing.T) {
	t.Parallel()
	first := mediaObservation("old", "", "old")
	second := mediaObservation("current", "", "current")

	selected := continuation.LatestMediaHandles([]trajectory.Item{first, second})
	if _, ok := selected["old"]; ok {
		t.Fatal("unlabelled stale media was selected")
	}
	if _, ok := selected["current"]; !ok {
		t.Fatal("unlabelled current media was not selected")
	}
}

func mediaObservation(id, source string, handles ...string) trajectory.Item {
	media := make([]trajectory.MediaRef, 0, len(handles))
	for _, handle := range handles {
		media = append(media, trajectory.MediaRef{Handle: handle, MIMEType: "image/jpeg", Source: source})
	}
	return trajectory.Item{
		ID: id, Kind: trajectory.KindObservation,
		Observation: &trajectory.ObservationMeta{
			Observer: "video", Source: source, Authority: trajectory.AuthorityObserver, Media: media,
		},
	}
}
