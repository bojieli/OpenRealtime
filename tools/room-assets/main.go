package main

import (
	"encoding/json"
	"github.com/bojieli/OpenRealtime/client/room"
	"github.com/bojieli/OpenRealtime/macos"
	"os"
)

func main() {
	for _, row := range []struct {
		file    string
		factory func() (*macos.NativeBundle, error)
	}{{"native-client-manifest.json", macos.NewNativeEffectsDeveloperBundle}, {"native-observer-client-manifest.json", macos.NewNativeObserverDeveloperBundle}} {
		b, err := row.factory()
		if err != nil {
			panic(err)
		}
		path := "macos/Sources/OpenRealtimeMac/Resources/" + row.file
		data, err := os.ReadFile(path)
		if err != nil {
			panic(err)
		}
		var resource map[string]any
		if err = json.Unmarshal(data, &resource); err != nil {
			panic(err)
		}
		resource["profile_fingerprint"] = b.Profile.Fingerprint
		resource["lock_fingerprint"] = b.Lock.Fingerprint
		resource["plan_fingerprint"] = b.Plan.Fingerprint
		resource["manifest_fingerprint"] = b.Manifest.Fingerprint
		data, err = json.MarshalIndent(resource, "", "  ")
		if err != nil {
			panic(err)
		}
		if err = os.WriteFile(path, append(data, '\n'), 0644); err != nil {
			panic(err)
		}
	}
	data, err := room.JSON()
	if err != nil {
		panic(err)
	}
	if err = os.WriteFile("macos/Sources/OpenRealtimeMac/Resources/room-scenarios.json", append(data, '\n'), 0644); err != nil {
		panic(err)
	}
}
