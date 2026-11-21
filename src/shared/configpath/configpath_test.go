package configpath

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultTOMLMissing(t *testing.T) {
	if p := DefaultTOML("definitely-not-here-4f8a.toml"); p != "" {
		t.Fatalf("DefaultTOML for a missing file = %q, want empty", p)
	}
}

func TestBesideExecutableMissing(t *testing.T) {
	if p := BesideExecutable("definitely-not-here-4f8a.toml"); p != "" {
		t.Fatalf("BesideExecutable for a missing file = %q, want empty", p)
	}
}

// TestDefaultTOMLCWDFallback covers the legacy behaviour: when the file is not
// next to the binary, the working directory is consulted.
func TestDefaultTOMLCWDFallback(t *testing.T) {
	dir := t.TempDir()
	name := "she11_test.toml"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("[general]\n"), 0644); err != nil {
		t.Fatal(err)
	}
	old, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(old)
	if p := DefaultTOML(name); p != name {
		t.Fatalf("DefaultTOML fallback = %q, want %q", p, name)
	}
}
