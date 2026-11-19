//go:build !windows

package commander

import "os"

func configureVT(out *os.File) (func(), error) { return func() {}, nil }
