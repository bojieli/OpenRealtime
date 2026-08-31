//go:build !darwin

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "companion process proof requires Darwin kernel evidence")
	os.Exit(1)
}
