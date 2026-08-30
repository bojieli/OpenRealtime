package media

import "testing"

func TestPlayableSpecBindsSourceAndDeterministicEncodedGeometry(t *testing.T) {
	request := AttestationRequest{
		Source: "screen", Width: 65, Height: 49,
		FirstFrameUS: 10_000, AttemptEndUS: 250_000,
		ExpectedFrameCount: 1, ExpectedFramePTSUSSHA256: digest([]byte("pts")),
		ExpectedOutputSHA256: digest([]byte("output")), ExpectedOutputBytes: 1,
	}
	valid := PlayableSpec{
		OutputSHA256: request.ExpectedOutputSHA256,
		Container:    "mp4", VideoCodec: "h264", PixelFormat: "yuv420p",
		Width: request.Width, Height: request.Height,
		EncodedWidth: 66, EncodedHeight: 50,
		GeometryPolicy: YUV420PPadRightBottomBlackToEvenV1,
		AudioCodec:     "aac", AudioSampleRateHz: reviewSampleRateHz, AudioChannels: reviewChannels,
		DurationUS: request.AttemptEndUS, AudioStartUS: 0, AudioEndUS: request.AttemptEndUS,
		VideoStartUS: request.FirstFrameUS, VideoEndUS: request.AttemptEndUS,
		VideoFrameCount:       request.ExpectedFrameCount,
		VideoFramePTSUSSHA256: request.ExpectedFramePTSUSSHA256,
	}
	if err := valid.validate(request); err != nil {
		t.Fatalf("valid odd geometry: %v", err)
	}
	evenRequest := request
	evenRequest.Width, evenRequest.Height = 64, 48
	even := valid
	even.Width, even.Height = 64, 48
	even.EncodedWidth, even.EncodedHeight = 64, 48
	if err := even.validate(evenRequest); err != nil {
		t.Fatalf("valid even geometry: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*PlayableSpec)
	}{
		{name: "crop", mutate: func(spec *PlayableSpec) { spec.EncodedWidth = 64 }},
		{name: "undeclared_pad", mutate: func(spec *PlayableSpec) { spec.GeometryPolicy = "" }},
		{name: "geometry_drift", mutate: func(spec *PlayableSpec) { spec.EncodedHeight = 52 }},
		{name: "source_drift", mutate: func(spec *PlayableSpec) { spec.Width = 66 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := valid
			test.mutate(&candidate)
			if err := candidate.validate(request); err == nil {
				t.Fatalf("geometry mutation unexpectedly validated: %+v", candidate)
			}
		})
	}
}
