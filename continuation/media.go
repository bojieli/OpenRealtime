package continuation

import (
	"strings"

	"github.com/bojieli/OpenRealtime/trajectory"
)

// LatestMediaHandles selects the media that represents the newest observation
// of each source in a trajectory.
//
// Observation text remains useful history, but repeatedly attaching every
// retained keyframe makes a realtime vision request slower and leaves the
// model choosing between stale and current pixels. All media references on
// the newest observation of a source are retained; references on older
// observations are omitted and therefore need not be resolved.
func LatestMediaHandles(items []trajectory.Item) map[string]struct{} {
	latest := make(map[string]int)
	for index, item := range items {
		if item.Kind != trajectory.KindObservation || item.Observation == nil {
			continue
		}
		for _, reference := range item.Observation.Media {
			latest[mediaSource(item.Observation, reference)] = index
		}
	}

	handles := make(map[string]struct{})
	for index, item := range items {
		if item.Kind != trajectory.KindObservation || item.Observation == nil {
			continue
		}
		for _, reference := range item.Observation.Media {
			if latest[mediaSource(item.Observation, reference)] == index {
				handles[reference.Handle] = struct{}{}
			}
		}
	}
	return handles
}

func mediaSource(observation *trajectory.ObservationMeta, reference trajectory.MediaRef) string {
	if source := strings.TrimSpace(reference.Source); source != "" {
		return "source:" + source
	}
	if source := strings.TrimSpace(observation.Source); source != "" {
		return "source:" + source
	}
	// Older producers did not always label a source. Grouping those frames by
	// observer still bounds the request instead of treating every opaque
	// handle as a separate visual stream.
	return "observer:" + strings.TrimSpace(observation.Observer)
}
