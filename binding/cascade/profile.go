package cascade

import (
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/bojieli/OpenRealtime/adapters/bysentence"
	"github.com/bojieli/OpenRealtime/cognition"
	"github.com/bojieli/OpenRealtime/continuation"
)

// speakableMinimum is how short the first spoken piece may be, matching the
// synthesiser's own cut so the measurement and the behaviour agree.
const speakableMinimum = 12

// stageTimer records how long each part of a turn took.
//
// It exists because a second of the visual path was unaccounted for and I had
// spent hours optimising the parts I could see. Measured separately, the
// decision was 21ms, the voice 1250ms and the synthesiser 770ms - and the path
// took 3066ms, so a third of it was in stages nobody had ever timed. Guessing
// which is the same mistake as guessing why a turn was silent, and it cost the
// same kind of afternoon.
//
// Wall-clock and per-stage, not a profile of the process: the question is
// never "where are the cycles" but "what was the person waiting through", and
// most of that is spent waiting on somebody else's machine.
type stageTimer struct {
	mu     sync.Mutex
	totals map[string]uint64
	counts map[string]uint64
}

func newStageTimer() *stageTimer {
	return &stageTimer{totals: map[string]uint64{}, counts: map[string]uint64{}}
}

// observe folds one measurement in, in nanoseconds.
func (timer *stageTimer) observe(stage string, tookNS uint64) {
	if timer == nil {
		return
	}
	timer.mu.Lock()
	defer timer.mu.Unlock()
	timer.totals[stage] += tookNS
	timer.counts[stage]++
}

// Report renders the mean of each stage, slowest first.
//
// The mean rather than a percentile, because the point is where the time went
// in total: a stage that takes 40ms twice a second costs more than one that
// takes 300ms once a turn, and a p50 hides exactly that.
func (timer *stageTimer) Report() string {
	if timer == nil {
		return ""
	}
	timer.mu.Lock()
	defer timer.mu.Unlock()
	type row struct {
		stage string
		mean  uint64
		count uint64
		total uint64
	}
	rows := make([]row, 0, len(timer.totals))
	for stage, total := range timer.totals {
		count := timer.counts[stage]
		if count == 0 {
			continue
		}
		rows = append(rows, row{stage, total / count, count, total})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].total > rows[j].total })
	var out strings.Builder
	for _, entry := range rows {
		out.WriteString(entry.stage + " " + strconv.FormatUint(entry.mean/1e6, 10) + "ms x" +
			strconv.FormatUint(entry.count, 10) + " (" +
			strconv.FormatUint(entry.total/1e6, 10) + "ms total)\n")
	}
	return out.String()
}

// phraseWatch times how much of a voice turn happens before there is something
// worth saying out loud.
//
// The turn report timed the voice as one stage, which answers "how long did the
// model take" and not "how long was the person waiting". Those differ by the
// tail: speech cannot start until publishAssistant has the whole text, so a
// model that produces a speakable phrase in 300ms and finishes in 800ms costs
// 500ms that nobody is reading and nobody is hearing.
type phraseWatch struct {
	runtime *runtime
	began   uint64
	text    strings.Builder
	token   uint64
	phrase  uint64
}

func (runtime *runtime) watchFirstPhrase(began uint64) *phraseWatch {
	return &phraseWatch{runtime: runtime, began: began}
}

func (watch *phraseWatch) observe(event cognition.StreamEvent) error {
	if watch == nil || event.Event.Kind != continuation.EventAssistantDelta || event.Event.Text == "" {
		return nil
	}
	if watch.token == 0 {
		watch.token = watch.runtime.scheduler.NowNS() - watch.began
	}
	if watch.phrase == 0 {
		watch.text.WriteString(event.Event.Text)
		// The same cut the synthesiser would make, so the number measures the
		// moment speech could have started rather than a moment near it.
		if pieces := bysentence.Split(watch.text.String(), speakableMinimum); len(pieces) > 1 {
			watch.phrase = watch.runtime.scheduler.NowNS() - watch.began
		}
	}
	return nil
}

func (watch *phraseWatch) report(turn *turnReport) {
	watch.reportAs(turn, "voice")
}

// reportAs names the stage, because the voice and the reasoner are the same
// measurement of two different models and reading one as the other has cost a
// day before.
func (watch *phraseWatch) reportAs(turn *turnReport, stage string) {
	if watch == nil {
		return
	}
	if watch.token > 0 {
		turn.stage(stage+"-first-token", watch.token)
	}
	if watch.phrase > 0 {
		turn.stage(stage+"-first-phrase", watch.phrase)
	}
}
