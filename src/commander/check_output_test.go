package commander

import (
	"fmt"
	"strings"
	"testing"
)

// TestRenderCheckTextExact keeps the --check-server text layout byte-stable:
// dynamic widths, " | " separators, a dashed rule under the header, short
// ok/error marks, the reachable summary and full error details below the
// table — the same shape as --list-client.
func TestRenderCheckTextExact(t *testing.T) {
	rows := []checkRow{
		{name: "relay-1", addr: "hk.example.com:2079", status: "ok", clients: 2, uptime: 86400},
		{name: "relay-2", addr: "jp.example.com:2079", status: "dial udp 1.2.3.4:2079: timeout"},
	}
	got := renderCheckText(rows, 1, 2)
	// Column widths: SERVER max(6,7)=7, ADDR max(4,19)=19, STATUS max(6,5)=6,
	// CLIENTS max(7,1)=7, UPTIME max(6,6)=6. stdout is not a terminal in
	// tests, so the terminal shrinker stays out of the way.
	line := "%-7s | %-19s | %-6s | %-7s | %-6s"
	header := fmt.Sprintf(line, "SERVER", "ADDR", "STATUS", "CLIENTS", "UPTIME")
	rule := fmt.Sprintf("%s-+-%s-+-%s-+-%s-+-%s",
		strings.Repeat("-", 7), strings.Repeat("-", 19), strings.Repeat("-", 6),
		strings.Repeat("-", 7), strings.Repeat("-", 6))
	row1 := fmt.Sprintf(line, "relay-1", "hk.example.com:2079", "ok", "2", "86400s")
	row2 := fmt.Sprintf(line, "relay-2", "jp.example.com:2079", "error", "-", "-")
	want := header + "\n" + rule + "\n" + row1 + "\n" + row2 + "\n" +
		"1 of 2 reachable\n" +
		"server error: relay-2 (jp.example.com:2079): dial udp 1.2.3.4:2079: timeout\n"
	if got != want {
		t.Fatalf("output mismatch\n got:\n%s\nwant:\n%s", got, want)
	}
}

// TestRenderCheckTextAllOk verifies the healthy-fleet output: no error marks
// and no trailing server-error lines.
func TestRenderCheckTextAllOk(t *testing.T) {
	rows := []checkRow{
		{name: "a", addr: "x.example.com:1", status: "ok", clients: 1, uptime: 5},
		{name: "b", addr: "y.example.com:2", status: "ok", clients: 0, uptime: 0},
	}
	got := renderCheckText(rows, 2, 2)
	if strings.Contains(got, "error") {
		t.Fatalf("no error expected in a healthy fleet:\n%s", got)
	}
	if !strings.Contains(got, "2 of 2 reachable") {
		t.Fatalf("reachable summary missing:\n%s", got)
	}
	// An ok relay with zero uptime still reports 0s, not a dash.
	if !strings.Contains(got, "| 0s") {
		t.Fatalf("expected 0s uptime for a fresh relay:\n%s", got)
	}
}

// TestRenderCheckTextAlignsCJK checks the invariant that matters: every
// rendered table line occupies the same number of terminal cells even when a
// relay name or address carries wide characters.
func TestRenderCheckTextAlignsCJK(t *testing.T) {
	rows := []checkRow{
		{name: "中继-1", addr: "hk1.example.com:2079", status: "ok", clients: 3, uptime: 120},
		{name: "relay-2", addr: "jp.example.com:2079", status: "auth failed: bad password"},
	}
	got := renderCheckText(rows, 1, 2)
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	// header, rule, 2 rows, then the summary and error lines follow.
	table := lines[:4]
	width := displayWidth(table[0])
	for _, line := range table[1:] {
		if w := displayWidth(line); w != width {
			t.Fatalf("line width %d != %d: %q", w, width, line)
		}
	}
	if !strings.Contains(got, " | ") || !strings.Contains(got, "-+-") {
		t.Fatalf("separators missing:\n%s", got)
	}
}
