//go:build !windows

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "acr-telemetry reads Windows shared memory; build with GOOS=windows GOARCH=amd64.")
	os.Exit(1)
}
