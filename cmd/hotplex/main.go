package main

import (
	"fmt"
	"os"

	_ "github.com/hrygo/hotplex/internal/worker/claudecode"
	_ "github.com/hrygo/hotplex/internal/worker/codexcli"
	_ "github.com/hrygo/hotplex/internal/worker/opencodeserver"
	"github.com/hrygo/hotplex/pkg/aep"
)

var (
	version   = "v1.52.0"
	buildTime = "unknown"
)

func main() {
	if handled, err := runInternalCLISurface(os.Args[1:]); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
		return
	}

	if isServiceRun() {
		runAsWindowsService(extractServiceConfig())
		return
	}

	rootCmd := newRootCmd()
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

func versionString() string { return version }

func newSessionID() string {
	return aep.NewSessionID()
}
