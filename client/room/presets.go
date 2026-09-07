// Package room shares the recording-room scenario catalog between browser and
// native presentations. Instructions and tool schemas come from the benchmark.
package room

import (
	"encoding/json"
	"github.com/bojieli/OpenRealtime/bench/scenario"
)

type Preset struct {
	Name         string                             `json:"name"`
	Note         string                             `json:"note"`
	Instructions string                             `json:"instructions"`
	Script       []scenario.Line                    `json:"script"`
	Tools        []scenario.FunctionToolDeclaration `json:"tools"`
}

func Presets() ([]Preset, error) {
	result := make([]Preset, 0, 12)
	for _, item := range scenario.Suite() {
		row := Preset{Name: item.Name, Note: item.Note, Instructions: item.Instructions, Script: item.Script, Tools: []scenario.FunctionToolDeclaration{}}
		for _, tool := range item.Tools {
			declaration, err := tool.FunctionDeclaration()
			if err != nil {
				return nil, err
			}
			row.Tools = append(row.Tools, declaration)
		}
		result = append(result, row)
	}
	return result, nil
}
func JSON() ([]byte, error) {
	presets, err := Presets()
	if err != nil {
		return nil, err
	}
	return json.Marshal(presets)
}
