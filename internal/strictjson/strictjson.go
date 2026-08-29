// Package strictjson rejects duplicate object keys and trailing values before
// typed decoding. encoding/json intentionally accepts duplicate keys, which is
// unsuitable for signed graph, lock, deployment, and descriptor artifacts.
package strictjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

func Validate(source []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.UseNumber()
	if err := consumeValue(decoder, "$"); err != nil {
		return err
	}
	if token, err := decoder.Token(); err == nil {
		return fmt.Errorf("trailing JSON token %v", token)
	} else if !errors.Is(err, io.EOF) {
		return fmt.Errorf("decode trailing JSON: %w", err)
	}
	return nil
}

func consumeValue(decoder *json.Decoder, path string) error {
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("decode JSON at %s: %w", path, err)
	}
	delimiter, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]struct{}{}
		for decoder.More() {
			keyToken, keyErr := decoder.Token()
			if keyErr != nil {
				return fmt.Errorf("decode JSON object at %s: %w", path, keyErr)
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("JSON object key at %s is not a string", path)
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate JSON key %q at %s", key, path)
			}
			seen[key] = struct{}{}
			if err := consumeValue(decoder, path+"."+key); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("decode JSON object at %s: %w", path, err)
		}
		if closing != json.Delim('}') {
			return fmt.Errorf("JSON object at %s closed with %v", path, closing)
		}
	case '[':
		index := 0
		for decoder.More() {
			if err := consumeValue(decoder, fmt.Sprintf("%s[%d]", path, index)); err != nil {
				return err
			}
			index++
		}
		closing, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("decode JSON array at %s: %w", path, err)
		}
		if closing != json.Delim(']') {
			return fmt.Errorf("JSON array at %s closed with %v", path, closing)
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q at %s", delimiter, path)
	}
	return nil
}
