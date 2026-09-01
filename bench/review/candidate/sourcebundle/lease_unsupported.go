//go:build !linux && !darwin && !windows

package sourcebundle

import (
	"errors"
	"os"
)

func lockSourceBundleFile(*os.File) error {
	return errors.New("candidate source exclusive leases are unsupported on this platform")
}
