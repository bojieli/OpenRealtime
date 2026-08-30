package browser

import (
	"testing"

	"github.com/bojieli/OpenRealtime/presentation/host"
)

func BenchmarkLockedClientBundle(b *testing.B) {
	benchmarks := []struct {
		name  string
		build func() (*Bundle, error)
	}{
		{name: "minimal-websocket", build: MinimalBundle},
		{name: "observer-websocket", build: ObserverDeveloperBundle},
		{name: "observer-webrtc", build: ObserverDeveloperWebRTCBundle},
		{name: "developer-websocket", build: DeveloperBundle},
		{name: "developer-webrtc", build: DeveloperWebRTCBundle},
	}
	for _, benchmark := range benchmarks {
		b.Run(benchmark.name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				bundle, err := benchmark.build()
				if err != nil {
					b.Fatal(err)
				}
				if bundle.Manifest.Fingerprint == "" || bundle.Plan.Fingerprint == "" {
					b.Fatal("bundle omitted immutable identity")
				}
			}
		})
	}
}

func BenchmarkCompileLockedClientBundle(b *testing.B) {
	catalogDigest, err := host.DefaultEffectsCatalogDigest()
	if err != nil {
		b.Fatal(err)
	}
	benchmarks := []struct {
		name  string
		build func() (*Bundle, error)
	}{
		{name: "minimal-websocket", build: buildMinimalBundle},
		{name: "observer-websocket", build: buildObserverDeveloperBundle},
		{name: "observer-webrtc", build: buildObserverDeveloperWebRTCBundle},
		{name: "developer-websocket", build: func() (*Bundle, error) {
			return buildDeveloperBundle(catalogDigest)
		}},
		{name: "developer-webrtc", build: func() (*Bundle, error) {
			return buildDeveloperWebRTCBundle(catalogDigest)
		}},
	}
	for _, benchmark := range benchmarks {
		b.Run(benchmark.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				bundle, err := benchmark.build()
				if err != nil {
					b.Fatal(err)
				}
				if bundle.Manifest.Fingerprint == "" || bundle.Plan.Fingerprint == "" {
					b.Fatal("compiled bundle omitted immutable identity")
				}
			}
		})
	}
}
