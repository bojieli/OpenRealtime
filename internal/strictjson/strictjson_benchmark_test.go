package strictjson_test

import (
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

var benchmarkValidationError error

func BenchmarkValidateCompactArray100K(b *testing.B) {
	var builder strings.Builder
	builder.Grow(200_001)
	builder.WriteByte('[')
	for index := 0; index < 100_000; index++ {
		if index > 0 {
			builder.WriteByte(',')
		}
		builder.WriteByte('0')
	}
	builder.WriteByte(']')
	source := []byte(builder.String())
	b.SetBytes(int64(len(source)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		benchmarkValidationError = strictjson.Validate(source)
		if benchmarkValidationError != nil {
			b.Fatal(benchmarkValidationError)
		}
	}
}

func BenchmarkValidateFFprobeLike100KFrames(b *testing.B) {
	source := makeFrameManifest(100_000)
	b.SetBytes(int64(len(source)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		benchmarkValidationError = strictjson.Validate(source)
		if benchmarkValidationError != nil {
			b.Fatal(benchmarkValidationError)
		}
	}
}

func BenchmarkRejectDepthOverLimit(b *testing.B) {
	defaults := strictjson.DefaultLimits()
	source := []byte(strings.Repeat("[", defaults.MaxDepth+1) + "0" + strings.Repeat("]", defaults.MaxDepth+1))
	b.SetBytes(int64(len(source)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		benchmarkValidationError = strictjson.Validate(source)
		if benchmarkValidationError == nil {
			b.Fatal("depth attack accepted")
		}
	}
}

func BenchmarkRejectLongKeyOverLimit(b *testing.B) {
	limits := strictjson.DefaultLimits()
	limits.MaxKeyBytes = 4 << 10
	source := []byte(`{"` + strings.Repeat("k", limits.MaxKeyBytes+1) + `":0}`)
	b.SetBytes(int64(len(source)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		benchmarkValidationError = strictjson.ValidateWithLimits(source, limits)
		if benchmarkValidationError == nil {
			b.Fatal("long key attack accepted")
		}
	}
}

func BenchmarkRejectEscapedDuplicateKey(b *testing.B) {
	source := []byte(`{"request_id":0,"request\u005fid":1}`)
	b.SetBytes(int64(len(source)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		benchmarkValidationError = strictjson.Validate(source)
		if benchmarkValidationError == nil {
			b.Fatal("decoded duplicate key accepted")
		}
	}
}

func BenchmarkValidateEscapedString1MiB(b *testing.B) {
	var builder strings.Builder
	builder.Grow(1 << 20)
	builder.WriteByte('"')
	for builder.Len() < (1<<20)-1 {
		builder.WriteString(`\n`)
	}
	builder.WriteByte('"')
	source := []byte(builder.String())
	b.SetBytes(int64(len(source)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		benchmarkValidationError = strictjson.Validate(source)
		if benchmarkValidationError != nil {
			b.Fatal(benchmarkValidationError)
		}
	}
}
