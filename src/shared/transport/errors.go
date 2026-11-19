package transport

import "fmt"

func errUnknownProto(p string) error {
	return fmt.Errorf("transport: unknown proto %q (want udp or tcp)", p)
}
