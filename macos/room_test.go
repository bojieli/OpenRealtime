package macos

import (
	"bytes"
	"encoding/json"
	"github.com/bojieli/OpenRealtime/client/room"
	"os"
	"testing"
)

func TestNativeRoomScenariosMatchCanonicalBrowserCatalog(t *testing.T) {
	want, err := room.JSON()
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile("Sources/OpenRealtimeMac/Resources/room-scenarios.json")
	if err != nil {
		t.Fatal(err)
	}
	var compact bytes.Buffer
	if err = json.Compact(&compact, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(compact.Bytes(), want) {
		t.Fatal("native room catalog drifted; run go run ./tools/room-assets")
	}
}
