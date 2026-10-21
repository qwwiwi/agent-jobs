// A sleep fixture without Apple's restricted code signature: Darwin sysctl can read its environment.
package main

import (
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) != 2 {
		os.Exit(2)
	}
	d, err := time.ParseDuration(os.Args[1] + "s")
	if err != nil || d < 0 {
		os.Exit(2)
	}
	if os.Getenv("AGENT_JOBS_TEST_SLEEP_IGNORE_TERM") == "1" {
		signal.Ignore(syscall.SIGTERM)
	}
	if ready := os.Getenv("AGENT_JOBS_TEST_SLEEP_READY"); ready != "" {
		if err := os.WriteFile(ready, nil, 0o600); err != nil {
			os.Exit(2)
		}
	}
	time.Sleep(d)
}
