package server

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestConcurrentSessionDumpPreservesBothDirections(t *testing.T) {
	folder := t.TempDir()
	dump, err := NewLogger(folder, "info").SessionDump("cmd", "test1234")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, direction := range []string{"in", "out"} {
		wg.Add(1)
		go func(direction string) {
			defer wg.Done()
			for range 100 {
				if err := dump.WriteLabeled(direction, []byte(direction)); err != nil {
					t.Error(err)
					return
				}
			}
		}(direction)
	}
	wg.Wait()
	dump.Close()
	dump.Close()
	b, err := os.ReadFile(filepath.Join(folder, "shell_dump", "cmd_test1234.log"))
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			t.Fatalf("invalid dump line: %q", line)
		}
		payload, err := base64.StdEncoding.DecodeString(fields[2])
		if err != nil || string(payload) != fields[1] {
			t.Fatalf("direction/payload mismatch: %q", line)
		}
		counts[fields[1]]++
	}
	if counts["in"] != 100 || counts["out"] != 100 {
		t.Fatalf("lost log chunks: %v", counts)
	}
}
