package perception

// A cancellation addresses an utterance within one session, including work
// still queued on another input lane. Keep a bounded FIFO just as the acoustic
// and generation policies do; new utterances use fresh stream identities.
const maximumCanceledAudioStreams = 512

type audioStreamAddress struct {
	session, stream string
}

type canceledAudioStreams struct {
	values map[audioStreamAddress]struct{}
	order  []audioStreamAddress
}

func (memory *canceledAudioStreams) has(session, stream string) bool {
	_, found := memory.values[audioStreamAddress{session: session, stream: stream}]
	return found
}

func (memory *canceledAudioStreams) add(session, stream string) {
	if stream == "" {
		return
	}
	address := audioStreamAddress{session: session, stream: stream}
	if _, found := memory.values[address]; found {
		return
	}
	if memory.values == nil {
		memory.values = make(map[audioStreamAddress]struct{})
	}
	memory.values[address] = struct{}{}
	memory.order = append(memory.order, address)
	if len(memory.order) > maximumCanceledAudioStreams {
		delete(memory.values, memory.order[0])
		memory.order = memory.order[1:]
	}
}
