package strictjson_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

func TestValidation(t *testing.T) {
	valid := []string{
		`{"a":[1,{"b":true}],"c":null}`,
		`"value"`,
		"  -12.34e+5 \r\n",
		`[0,-0,1.0,2E-2,false,true,null]`,
		`{"":"","escaped":"\\\"\/\b\f\n\r\t","unicode":"\u20ac"}`,
		`{"emoji":"\uD83D\uDE00","raw":"世界"}`,
		`{"é":1,"e\u0301":2}`,
	}
	for _, source := range valid {
		t.Run("valid_"+shortName(source), func(t *testing.T) {
			assertValid(t, []byte(source), strictjson.Limits{})
		})
	}

	invalid := []string{
		``,
		`{"a":1,"a":2}`,
		`{"a":{"b":1,"b":2}}`,
		`{} {}`,
		`{"a":`,
		`{"a" 1}`,
		`{"a":1,}`,
		`[1,]`,
		`[,1]`,
		`[1 2]`,
		`01`,
		`-`,
		`1.`,
		`1e`,
		`+1`,
		`NaN`,
		`TRUE`,
		`"unterminated`,
		`"bad\xescape"`,
		"\"raw\ncontrol\"",
	}
	for _, source := range invalid {
		t.Run("invalid_"+shortName(source), func(t *testing.T) {
			assertInvalid(t, []byte(source), strictjson.Limits{})
		})
	}
}

func TestUnicodeAndDecodedDuplicateValidation(t *testing.T) {
	valid := [][]byte{
		[]byte(`"\u0000"`),
		[]byte(`"\uD800\uDC00"`),
		[]byte(`{"\uD83D\uDE00":1,"different":2}`),
		[]byte(`{"a\nb":1,"a\tb":2}`),
	}
	for _, source := range valid {
		assertValid(t, source, strictjson.Limits{})
	}

	invalid := [][]byte{
		[]byte(`"\uD800"`),
		[]byte(`"\uDBFF"`),
		[]byte(`"\uDC00"`),
		[]byte(`"\uDFFF"`),
		[]byte(`"\uD800\uD800"`),
		[]byte(`"\uD800\uE000"`),
		[]byte(`"\uD800\uZZZZ"`),
		[]byte(`{"a":1,"\u0061":2}`),
		[]byte(`{"\/":1,"/":2}`),
		[]byte(`{"\uD83D\uDE00":1,"😀":2}`),
		[]byte(`{"a\nb":1,"a\u000Ab":2}`),
		{'"', 0xff, '"'},
		{'"', 0xc0, 0xaf, '"'},
		{'"', 0xe2, 0x82, '"'},
		{'{', '"', 0x80, '"', ':', '0', '}'},
		{0xef, 0xbb, 0xbf, 'n', 'u', 'l', 'l'},
	}
	for index, source := range invalid {
		t.Run(shortName(string(source)), func(t *testing.T) {
			if err := strictjson.Validate(source); err == nil {
				t.Fatalf("invalid Unicode case %d accepted: %x", index, source)
			}
		})
	}
}

func TestConfigurableLimitsExactBoundaries(t *testing.T) {
	tests := []struct {
		name    string
		limits  func() strictjson.Limits
		atLimit []byte
		over    []byte
	}{
		{
			name: "input_bytes",
			limits: func() strictjson.Limits {
				limits := strictjson.DefaultLimits()
				limits.MaxInputBytes = 4
				return limits
			},
			atLimit: []byte(`null`),
			over:    []byte(` null`),
		},
		{
			name: "depth",
			limits: func() strictjson.Limits {
				limits := strictjson.DefaultLimits()
				limits.MaxDepth = 2
				return limits
			},
			atLimit: []byte(`[[0]]`),
			over:    []byte(`[[[0]]]`),
		},
		{
			name: "tokens",
			limits: func() strictjson.Limits {
				limits := strictjson.DefaultLimits()
				limits.MaxTokens = 3
				return limits
			},
			atLimit: []byte(`{"a":0}`),
			over:    []byte(`{"a":[0]}`),
		},
		{
			name: "object_members",
			limits: func() strictjson.Limits {
				limits := strictjson.DefaultLimits()
				limits.MaxObjectMembers = 2
				return limits
			},
			atLimit: []byte(`{"a":0,"b":1}`),
			over:    []byte(`{"a":0,"b":1,"c":2}`),
		},
		{
			name: "array_elements",
			limits: func() strictjson.Limits {
				limits := strictjson.DefaultLimits()
				limits.MaxArrayElements = 2
				return limits
			},
			atLimit: []byte(`[0,1]`),
			over:    []byte(`[0,1,2]`),
		},
		{
			name: "decoded_key_bytes",
			limits: func() strictjson.Limits {
				limits := strictjson.DefaultLimits()
				limits.MaxKeyBytes = 3
				return limits
			},
			atLimit: []byte(`{"abc":0}`),
			over:    []byte(`{"abcd":0}`),
		},
		{
			name: "escaped_decoded_key_bytes",
			limits: func() strictjson.Limits {
				limits := strictjson.DefaultLimits()
				limits.MaxKeyBytes = 1
				return limits
			},
			atLimit: []byte(`{"\u0061":0}`),
			over:    []byte(`{"\u0061b":0}`),
		},
		{
			name: "total_decoded_key_bytes",
			limits: func() strictjson.Limits {
				limits := strictjson.DefaultLimits()
				limits.MaxTotalKeyBytes = 4
				return limits
			},
			atLimit: []byte(`{"ab":0,"cd":1}`),
			over:    []byte(`{"ab":0,"cde":1}`),
		},
		{
			name: "structural_work",
			limits: func() strictjson.Limits {
				limits := strictjson.DefaultLimits()
				limits.MaxWorkBytes = 5
				return limits
			},
			atLimit: []byte(`null`),
			over:    []byte(` null`),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertValid(t, test.atLimit, test.limits())
			assertInvalid(t, test.over, test.limits())
		})
	}
}

func TestDefaultsAdmitLargeFrameManifest(t *testing.T) {
	const frameCount = 100_000
	manifest := makeFrameManifest(frameCount)
	assertValid(t, manifest, strictjson.Limits{})

	limits := strictjson.DefaultLimits()
	limits.MaxArrayElements = frameCount
	assertValid(t, manifest, limits)
	assertInvalid(t, makeFrameManifest(frameCount+1), limits)
}

func TestDefaultDepthAndKeyBoundaries(t *testing.T) {
	defaults := strictjson.DefaultLimits()
	depthAtLimit := []byte(strings.Repeat("[", defaults.MaxDepth) + "0" + strings.Repeat("]", defaults.MaxDepth))
	depthOverLimit := []byte(strings.Repeat("[", defaults.MaxDepth+1) + "0" + strings.Repeat("]", defaults.MaxDepth+1))
	assertValid(t, depthAtLimit, strictjson.Limits{})
	assertInvalid(t, depthOverLimit, strictjson.Limits{})

	keyAtLimit := []byte(`{"` + strings.Repeat("k", defaults.MaxKeyBytes) + `":0}`)
	keyOverLimit := []byte(`{"` + strings.Repeat("k", defaults.MaxKeyBytes+1) + `":0}`)
	assertValid(t, keyAtLimit, strictjson.Limits{})
	assertInvalid(t, keyOverLimit, strictjson.Limits{})
}

func TestErrorsAreBoundedAndDoNotContainCumulativePaths(t *testing.T) {
	key := strings.Repeat("sensitive-key-material", 100)
	duplicate := []byte(`{"` + key + `":0,"` + key + `":1}`)
	err := strictjson.Validate(duplicate)
	if err == nil {
		t.Fatal("duplicate key accepted")
	}
	message := err.Error()
	if len(message) > 192 {
		t.Fatalf("diagnostic is not bounded: %d bytes: %q", len(message), message)
	}
	if strings.Contains(message, key) {
		t.Fatal("diagnostic disclosed the duplicate key")
	}

	deep := []byte(strings.Repeat(`{"x":`, 200) + `{"a":0,"\u0061":1}` + strings.Repeat("}", 200))
	err = strictjson.Validate(deep)
	if err == nil {
		t.Fatal("deep duplicate key accepted")
	}
	if len(err.Error()) > 192 {
		t.Fatalf("deep diagnostic accumulated a path: %d bytes", len(err.Error()))
	}
}

func TestZeroLimitsUseDefaultsAndNegativeLimitsFail(t *testing.T) {
	assertValid(t, []byte(`{"a":[0]}`), strictjson.Limits{})

	negative := []strictjson.Limits{
		{MaxInputBytes: -1},
		{MaxDepth: -1},
		{MaxTokens: -1},
		{MaxObjectMembers: -1},
		{MaxArrayElements: -1},
		{MaxKeyBytes: -1},
		{MaxTotalKeyBytes: -1},
		{MaxWorkBytes: -1},
	}
	for index, limits := range negative {
		if err := strictjson.ValidateWithLimits([]byte(`null`), limits); err == nil {
			t.Fatalf("negative limits case %d accepted", index)
		}
	}
}

func TestRegressionCorpusIsDeterministic(t *testing.T) {
	corpus := [][]byte{
		[]byte(strings.Repeat("[", 10_000)),
		[]byte(strings.Repeat(`{"a":`, 1_000)),
		[]byte(`{"a":0,"\u0061":1}`),
		[]byte(`"\uD800\u0000"`),
		[]byte(`[0,1,2,3,4,5,6,7,8,9,]`),
		{'"', 0xf4, 0x90, 0x80, 0x80, '"'},
		{'"', 0xed, 0xa0, 0x80, '"'},
	}
	for index, source := range corpus {
		first := strictjson.Validate(source)
		second := strictjson.Validate(source)
		if (first == nil) != (second == nil) {
			t.Fatalf("case %d produced nondeterministic acceptance", index)
		}
		if first != nil && first.Error() != second.Error() {
			t.Fatalf("case %d produced nondeterministic errors: %q != %q", index, first, second)
		}
	}
}

func FuzzValidate(f *testing.F) {
	for _, seed := range [][]byte{
		[]byte(`null`),
		[]byte(`{"a":[0,true,"x"]}`),
		[]byte(`{"a":0,"\u0061":1}`),
		[]byte(`"\uD83D\uDE00"`),
		{'"', 0xff, '"'},
		[]byte(strings.Repeat("[", 300) + "0" + strings.Repeat("]", 300)),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, source []byte) {
		first := strictjson.Validate(source)
		second := strictjson.Validate(source)
		if (first == nil) != (second == nil) {
			t.Fatal("validation result changed between identical calls")
		}
		if first != nil && first.Error() != second.Error() {
			t.Fatalf("validation diagnostic changed: %q != %q", first, second)
		}
		if first == nil && !json.Valid(source) {
			t.Fatalf("strict validator accepted syntax rejected by encoding/json: %x", source)
		}
	})
}

func assertValid(t *testing.T, source []byte, limits strictjson.Limits) {
	t.Helper()
	if err := strictjson.ValidateWithLimits(source, limits); err != nil {
		t.Fatalf("valid JSON rejected (%d bytes): %v", len(source), err)
	}
}

func assertInvalid(t *testing.T, source []byte, limits strictjson.Limits) {
	t.Helper()
	if err := strictjson.ValidateWithLimits(source, limits); err == nil {
		t.Fatalf("invalid JSON accepted (%d bytes): %.160q", len(source), source)
	}
}

func makeFrameManifest(frameCount int) []byte {
	var builder strings.Builder
	builder.Grow(12 + frameCount*10)
	builder.WriteString(`{"frames":[`)
	for index := 0; index < frameCount; index++ {
		if index > 0 {
			builder.WriteByte(',')
		}
		builder.WriteString(`{"pts":0}`)
	}
	builder.WriteString(`]}`)
	return []byte(builder.String())
}

func shortName(value string) string {
	if len(value) > 24 {
		value = value[:24]
	}
	var builder strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			builder.WriteRune(r)
		default:
			builder.WriteByte('_')
		}
	}
	if builder.Len() == 0 {
		return "empty"
	}
	return builder.String()
}
