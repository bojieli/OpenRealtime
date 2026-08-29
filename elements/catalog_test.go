package elements_test

import (
	"testing"

	"github.com/bojieli/OpenRealtime/elements"
)

func TestStandardCatalogsConstruct(t *testing.T) {
	if _, err := elements.Catalog(); err != nil {
		t.Fatal(err)
	}
	if _, err := elements.RuntimeRegistry(); err != nil {
		t.Fatal(err)
	}
}
