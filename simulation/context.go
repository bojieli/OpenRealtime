package simulation

import (
	"fmt"
	"strings"
)

// LargeContext builds the shared document the debate scenario argues over.
//
// It is generated rather than checked in so the needle's position is
// deterministic and the size is a knob rather than a file. What matters is
// that it is long enough that the interesting figure cannot be found by
// reading the first paragraph, and that it reads like a real specification -
// a document full of "lorem ipsum" would let a model do well by pattern
// matching on the one section that contains prose.
//
// The needle appears exactly once, in the middle, phrased as a measurement
// among other measurements. Nothing marks it as special.
func LargeContext(needle string) string {
	var builder strings.Builder
	builder.WriteString(header)

	// The needle sits in the middle section. Front-loading it would make the
	// scenario pass on a model that only reads the beginning of its context,
	// and putting it last would make it pass on one that only reads the end.
	const sections = 24
	needleSection := sections / 2

	for index := 1; index <= sections; index++ {
		fmt.Fprintf(&builder, "\n\n## %d. %s\n\n", index, sectionTitles[index%len(sectionTitles)])
		builder.WriteString(sectionBody(index))
		if index == needleSection {
			fmt.Fprintf(&builder,
				"\n\nUnder the admission policy above, the p99 admission latency measured on "+
					"the reference deployment is %s, which is the figure the capacity model is "+
					"built on. The p50 is under two milliseconds and the p999 is forty-one "+
					"milliseconds; neither is used for sizing.", needle)
		}
	}
	builder.WriteString("\n\n## Open questions\n\n" +
		"Whether the design is ready to ship is the subject of this review. " +
		"The figures above are measured; the conclusions drawn from them are not.\n")
	return builder.String()
}

const header = `# Realtime Admission and Scheduling Specification, revision 7

This document specifies admission control, scheduling, and backpressure for a
realtime media service. Every figure in it was measured on the reference
deployment described in section 1 unless stated otherwise. Figures given
without a percentile are medians.`

var sectionTitles = []string{
	"Reference deployment",
	"Admission control",
	"Scheduling classes",
	"Backpressure and shedding",
	"Media transport",
	"Jitter and playout",
	"Failure domains",
	"Capacity model",
	"Observability",
	"Upgrade and rollback",
	"Security boundaries",
	"Interoperability",
}

// sectionBody produces plausible specification prose with figures in it.
//
// The figures vary by section so that a model arguing from the document has
// specifics to cite, and so that citing the wrong one is visibly wrong rather
// than merely unconvincing.
func sectionBody(index int) string {
	base := index * 7
	return fmt.Sprintf(
		"The service admits a session only when the scheduler can guarantee its class for the "+
			"whole of the session's expected lifetime. Admission is evaluated once, at the start, "+
			"and never re-evaluated: a session that has been admitted is not shed, because shedding "+
			"a live conversation is worse than refusing one that has not started.\n\n"+
			"The reference deployment sustains %d concurrent sessions per node at %d%% CPU headroom, "+
			"with a measured tail of %d milliseconds at the ninety-fifth percentile and %d "+
			"milliseconds at the ninety-ninth. Beyond %d sessions the tail grows superlinearly and "+
			"the node is considered saturated regardless of average utilisation.\n\n"+
			"Backpressure is applied at the ingress rather than at the scheduler. When the ingress "+
			"queue exceeds %d frames, new sessions are refused with a retriable status; existing "+
			"sessions are unaffected. The queue is bounded at %d frames, above which frames are "+
			"dropped oldest-first, on the reasoning that the newest audio is the audio somebody is "+
			"currently waiting on.\n\n"+
			"Interrupt-class work reserves %d%% of the queue at all times. A routine burst that "+
			"filled the queue would otherwise be able to delay an interrupt, and an interrupt that "+
			"arrives late has already failed at the thing it exists to do.",
		120+base, 30+index%20, 40+base, 90+base*2, 150+base, 800+base*10, 2000+base*20, 10+index%15)
}
