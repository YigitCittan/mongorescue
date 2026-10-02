//go:build ignore

// Command updatescript prints the desktop app's update script
// (desktop.UpdateScript) for the browser suite, which injects it into the
// dashboard the way the desktop app does. The ignore build constraint keeps it
// out of go build ./..., go vet ./... and go test ./...; run it explicitly:
//
//	go run ./e2e/updatescript/main.go > update-script.js
package main

import (
	"fmt"

	"github.com/yigitcittan/mongorescue/internal/desktop"
)

func main() {
	fmt.Print(desktop.UpdateScript())
}
