package commander

import (
	"fmt"
	"strings"
	"testing"
)

func TestDisplayWidth(t *testing.T) {
	cases := []struct {
		text string
		want int
	}{
		{"", 0},
		{"abc", 3},
		{"中文", 4},
		{"a中", 3},
		{"e\u0301", 1}, // e + combining acute
		{"\U0001F642", 2},
		{"WINDOWS10\\Administrator", 23},
	}
	for _, tc := range cases {
		if got := displayWidth(tc.text); got != tc.want {
			t.Fatalf("displayWidth(%q) = %d, want %d", tc.text, got, tc.want)
		}
	}
}

func TestFitCell(t *testing.T) {
	cases := []struct {
		text  string
		width int
		want  string
	}{
		{"abc", 5, "abc  "},
		{"abcde", 5, "abcde"},
		{"abcdef", 5, "abcd\u2026"},
		{"中文", 4, "中文"},
		{"中文abc", 4, "中\u2026 "},
		{"a\tb", 3, "a b"},
		{"x", 0, ""},
	}
	for _, tc := range cases {
		got := fitCell(tc.text, tc.width)
		if got != tc.want {
			t.Fatalf("fitCell(%q, %d) = %q, want %q", tc.text, tc.width, got, tc.want)
		}
		if tc.width > 0 && displayWidth(got) != tc.width {
			t.Fatalf("fitCell(%q, %d) width = %d, want %d", tc.text, tc.width, displayWidth(got), tc.width)
		}
	}
}

// TestRenderTableExact keeps the everyday row (one Windows box) byte-stable:
// dynamic widths, " | " separators and a dashed rule under the header.
func TestRenderTableExact(t *testing.T) {
	rows := [][]string{{
		"k7jfe5qf", "WINDOWS10", `WINDOWS10\Administrator`, "windows/amd64", "ghost", "pwsh,powershell,cmd",
	}}
	got, truncated := renderTable(
		[]string{"ID", "HOSTNAME", "USER", "OS/ARCH", "VIA", "SHELLS"},
		rows,
		[]int{10, 28, 28, 16, 12, 36},
		120,
	)
	if truncated {
		t.Fatalf("nothing should be truncated: %q", got)
	}
	header := fmt.Sprintf("%-8s | %-9s | %-23s | %-13s | %-5s | %-19s",
		"ID", "HOSTNAME", "USER", "OS/ARCH", "VIA", "SHELLS")
	rule := fmt.Sprintf("%s-+-%s-+-%s-+-%s-+-%s-+-%s",
		strings.Repeat("-", 8), strings.Repeat("-", 9), strings.Repeat("-", 23),
		strings.Repeat("-", 13), strings.Repeat("-", 5), strings.Repeat("-", 19))
	row := fmt.Sprintf("%-8s | %-9s | %-23s | %-13s | %-5s | %-19s",
		"k7jfe5qf", "WINDOWS10", `WINDOWS10\Administrator`, "windows/amd64", "ghost", "pwsh,powershell,cmd")
	want := header + "\n" + rule + "\n" + row + "\n"
	if got != want {
		t.Fatalf("table mismatch\n got:\n%s\nwant:\n%s", got, want)
	}
}

// TestRenderTableAlignsCJK checks the invariant that matters: every rendered
// line occupies the same number of terminal cells even with wide characters.
func TestRenderTableAlignsCJK(t *testing.T) {
	rows := [][]string{
		{"k7jfe5qf", "主机-A", `域\用户`, "windows/amd64", "ghost", "cmd"},
		{"abcd1234", "srv", `CORP\deploy`, "linux/arm64", "relay-b", "fish,sh"},
	}
	got, _ := renderTable(
		[]string{"ID", "HOSTNAME", "USER", "OS/ARCH", "VIA", "SHELLS"},
		rows,
		[]int{10, 28, 28, 16, 12, 36},
		120,
	)
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if len(lines) != 2+len(rows) {
		t.Fatalf("expected header, rule and %d rows, got %d lines", len(rows), len(lines))
	}
	width := displayWidth(lines[0])
	for _, line := range lines[1:] {
		if w := displayWidth(line); w != width {
			t.Fatalf("line width %d != %d: %q", w, width, line)
		}
	}
	if !strings.Contains(got, " | ") || !strings.Contains(got, "-+-") {
		t.Fatalf("separators missing:\n%s", got)
	}
}

// TestRenderTableFitsTerminal verifies the safety net for narrow terminals.
func TestRenderTableFitsTerminal(t *testing.T) {
	rows := [][]string{
		{"k7jfe5qf", "srv-with-a-very-long-hostname", `CORP\svc_account_with_a_long_name`,
			"windows/amd64", "relay-with-a-long-name", "pwsh,powershell,cmd,bash"},
	}
	got, truncated := renderTable(
		[]string{"ID", "HOSTNAME", "USER", "OS/ARCH", "VIA", "SHELLS"},
		rows,
		[]int{10, 28, 28, 16, 12, 36},
		60,
	)
	if !truncated {
		t.Fatalf("narrow terminal should truncate something:\n%s", got)
	}
	for _, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
		if w := displayWidth(line); w > 60 {
			t.Fatalf("line exceeds 60 cells (%d): %q", w, line)
		}
	}
}

// TestRenderTableKeepsShortColumns checks that an 80-column terminal trims the
// long free-form columns (USER, SHELLS) but leaves HOSTNAME, OS/ARCH and VIA
// intact.
func TestRenderTableKeepsShortColumns(t *testing.T) {
	rows := [][]string{{
		"k7jfe5qf", "WINDOWS10", `WINDOWS10\Administrator`, "windows/amd64", "ghost", "pwsh,powershell,cmd",
	}}
	got, truncated := renderTable(
		[]string{"ID", "HOSTNAME", "USER", "OS/ARCH", "VIA", "SHELLS"},
		rows,
		[]int{10, 28, 28, 16, 12, 36},
		80,
	)
	if !truncated {
		t.Fatalf("80 columns should trim the long columns:\n%s", got)
	}
	for _, keep := range []string{"WINDOWS10", "windows/amd64", "ghost"} {
		if !strings.Contains(got, keep) {
			t.Fatalf("short column value %q was trimmed:\n%s", keep, got)
		}
	}
	for _, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
		if w := displayWidth(line); w > 80 {
			t.Fatalf("line exceeds 80 cells (%d): %q", w, line)
		}
	}
	t.Logf("80-column rendering:\n%s", got)
}

func TestRenderTableCapsShortenWithEllipsis(t *testing.T) {
	rows := [][]string{{
		"k7jfe5qf", "WINDOWS10", `WINDOWS10\Administrator`, "windows/amd64", "ghost", "cmd",
	}}
	got, truncated := renderTable(
		[]string{"ID", "HOSTNAME", "USER", "OS/ARCH", "VIA", "SHELLS"},
		rows,
		[]int{8, 8, 8, 8, 5, 6},
		0,
	)
	if !truncated {
		t.Fatalf("tight caps should truncate:\n%s", got)
	}
	if !strings.Contains(got, "\u2026") {
		t.Fatalf("truncated cell has no ellipsis:\n%s", got)
	}
	for _, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
		if w := displayWidth(line); w != 8+8+8+8+5+6+3*5 {
			t.Fatalf("line width %d unexpected: %q", w, line)
		}
	}
}
