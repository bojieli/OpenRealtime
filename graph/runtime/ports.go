package runtime

import (
	"fmt"

	"github.com/bojieli/OpenRealtime/element"
)

type portSet struct {
	inputs  map[string]*inputPort
	outputs map[string]*outputPort
}

func (ports *portSet) Input(name string) (element.InputPort, error) {
	port, found := ports.inputs[name]
	if !found {
		return nil, fmt.Errorf("element has no input port %q", name)
	}
	return port, nil
}

func (ports *portSet) Output(name string) (element.OutputPort, error) {
	port, found := ports.outputs[name]
	if !found {
		return nil, fmt.Errorf("element has no output port %q", name)
	}
	return port, nil
}
