package gemini

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func FuzzCredentialScanner(f *testing.F) {
	middle := len(testAPIKey) / 2
	half1, half2 := testAPIKey[:middle], testAPIKey[middle:]
	escaped := strings.ReplaceAll(testAPIKey, "-", `\u002d`)
	for _, seed := range [][]byte{
		[]byte(`{"value":"` + testAPIKey + `"}`),
		[]byte(`{"value":"` + escaped + `"}`),
		[]byte(`{"a":"` + half1 + `","b":"` + half2 + `"}`),
		[]byte(`{"` + half1 + `":1,"` + half2 + `":2}`),
		[]byte(`{"outer":"prefix {\"a\":\"` + half1 + `\",\"b\":\"` + half2 + `\"} suffix"}`),
		[]byte(`{"unicode":"\ud83d\ude00","value":"safe"}`),
		[]byte(`{"malformed":"unterminated}`),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, payload []byte) {
		if len(payload) > 64<<10 {
			t.Skip()
		}
		scanner, err := newJSONCredentialScanner(testAPIKey)
		if err != nil {
			t.Fatal(err)
		}
		defer scanner.wipe()
		contains := scanner.containsJSON(payload)
		if !json.Valid(payload) {
			if !contains {
				t.Fatal("malformed JSON passed the fail-closed credential boundary")
			}
			return
		}
		if bytes.Contains(payload, []byte(testAPIKey)) && !contains {
			t.Fatal("literal credential was missed")
		}
		if referenceDecodedTokenContainsCredential(payload, testAPIKey) && !contains {
			t.Fatal("reference decoded-token stream found a credential the scanner missed")
		}
	})
}

func referenceDecodedTokenContainsCredential(payload []byte, credential string) bool {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	all := strings.Builder{}
	for {
		token, err := decoder.Token()
		if err != nil {
			break
		}
		if delimiter, ok := token.(json.Delim); ok {
			_ = delimiter
			continue
		}
		text := ""
		switch value := token.(type) {
		case string:
			text = value
		case json.Number:
			text = value.String()
		case bool:
			if value {
				text = "true"
			} else {
				text = "false"
			}
		case nil:
			text = "null"
		}
		if text == "" {
			continue
		}
		all.WriteString(text)
	}
	return strings.Contains(all.String(), credential)
}
