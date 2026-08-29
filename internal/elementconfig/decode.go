// Package elementconfig decodes the strict, object-shaped values supplied to
// element factories. Topology never embeds these values; mount receives the
// separately resolved artifact for one node.
package elementconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"

	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

// Decode rejects duplicate keys, non-object roots, unknown fields, trailing
// values, and non-pointer destinations. An omitted artifact is the empty
// object, which lets elements with no settings share this path.
func Decode(source json.RawMessage, destination any) error {
	if destination == nil {
		return errors.New("element configuration destination is nil")
	}
	typeOf := reflect.TypeOf(destination)
	if typeOf.Kind() != reflect.Pointer || reflect.ValueOf(destination).IsNil() {
		return fmt.Errorf("element configuration destination must be a non-nil pointer, got %T", destination)
	}
	if len(source) == 0 {
		source = json.RawMessage("{}")
	}
	if err := strictjson.Validate(source); err != nil {
		return err
	}
	trimmed := bytes.TrimSpace(source)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return errors.New("element configuration must be a JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return errors.New("element configuration has a trailing JSON value")
	} else if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}
