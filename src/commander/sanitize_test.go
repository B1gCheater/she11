package commander

import (
	"strings"
	"testing"
)

func TestSanitizeStripsTerminalNoiseAndKeepsText(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"clear screen and cursor home", "\x1b[2J\x1b[m\x1b[H{\"sum\":5,\"ok\":true}\r\n", "{\"sum\":5,\"ok\":true}\n"},
		{"osc window title with BEL", "\x1b]0;C:\\pwsh.exe\x07OUT\r\n", "OUT\n"},
		{"osc window title with ST", "\x1b]0;title\x1b\\OUT\r\n", "OUT\n"},
		{"show cursor and sgr reset", "\x1b[?25h\x1b[mDONE\r\n", "DONE\n"},
		{"lone CR becomes newline", "A\rB", "A\nB"},
		{"tabs preserved", "col1\tcol2", "col1\tcol2"},
		{"utf8 untouched", "中文测试 OK", "中文测试 OK"},
		{"utf8 inside colored run", "\x1b[31m错误\x1b[0m", "错误"},
		{"backspace controls dropped", "a\x08b", "ab"},
		{"del dropped", "a\x7fb", "ab"},
		{"no escapes unchanged", "plain output\nsecond line\n", "plain output\nsecond line\n"},
		{"empty", "", ""},
		{"charset designator", "\x1b(Btext", "text"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cleanOutput(tt.in); got != tt.want {
				t.Fatalf("cleanOutput(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if strings.ContainsRune(cleanOutput(tt.in), '\x1b') {
				t.Fatalf("escape survived in %q", tt.in)
			}
		})
	}
}

// A CSI sequence split across two network chunks must not leak a partial
// escape into the first response, nor lose the text that follows in the second.
func TestSanitizeCarriesSplitSequenceAcrossReads(t *testing.T) {
	first := sanitizeTerminal([]byte("HEAD\x1b[2"))
	if first.text != "HEAD" {
		t.Fatalf("first chunk = %q, want %q", first.text, "HEAD")
	}
	if string(first.tail) != "\x1b[2" {
		t.Fatalf("tail = %q, want the incomplete sequence carried", first.tail)
	}
	// Next read starts from the carried tail plus new bytes.
	second := sanitizeTerminal(append(append([]byte{}, first.tail...), []byte("J\x1b[HTAIL")...))
	if second.text != "TAIL" {
		t.Fatalf("second chunk = %q, want %q", second.text, "TAIL")
	}
	if len(second.tail) != 0 {
		t.Fatalf("unexpected carried tail %q", second.tail)
	}
	if first.text+second.text != "HEADTAIL" {
		t.Fatalf("concatenated reads = %q, want HEADTAIL", first.text+second.text)
	}
}

// An OSC title split mid-parameter is the trickiest case: BEL vs ST cannot be
// decided yet, so it must be carried rather than guessed.
func TestSanitizeCarriesSplitOSCTitle(t *testing.T) {
	first := sanitizeTerminal([]byte("A\x1b]0;win"))
	if first.text != "A" {
		t.Fatalf("first chunk = %q, want %q", first.text, "A")
	}
	second := sanitizeTerminal(append(append([]byte{}, first.tail...), []byte("dow\x07B")...))
	if second.text != "B" {
		t.Fatalf("second chunk = %q, want %q", second.text, "B")
	}
}

func TestSanitizeNeverSplitsMultibyteRune(t *testing.T) {
	// Each prefix must still yield valid, prefix-consistent text: the cleaner
	// only ever cuts at ASCII control bytes, never inside a UTF-8 character.
	s := "中\x1b[1m文\x1b[0m测试"
	full := cleanOutput(s)
	if full != "中文测试" {
		t.Fatalf("cleanOutput = %q, want 中文测试", full)
	}
	for i := 1; i <= len(s); i++ {
		got := sanitizeTerminal([]byte(s[:i]))
		for _, r := range got.text {
			if r == 0xFFFD {
				t.Fatalf("replacement char at prefix %d: %q", i, got.text)
			}
		}
	}
}

func TestBudgetOutputKeepsHeadAndTail(t *testing.T) {
	// The head usually holds the interesting output and the tail the error.
	s := "HEAD_" + strings.Repeat("x", 5000) + "_TAIL"
	out, omitted, cut := budgetOutput(s, 300)
	if !cut {
		t.Fatal("expected truncation")
	}
	if !strings.HasPrefix(out, "HEAD_") || !strings.HasSuffix(out, "_TAIL") {
		t.Fatalf("budget dropped a boundary: prefix=%q suffix=%q", out[:20], out[len(out)-20:])
	}
	if len(out) > 300 {
		t.Fatalf("budget exceeded: len=%d want <=300", len(out))
	}
	if omitted <= 0 || !strings.Contains(out, "omitted") {
		t.Fatalf("omitted count not reported: omitted=%d out=%q", omitted, out[len(out)/2-40:len(out)/2+40])
	}
}

func TestBudgetOutputNoOpUnderLimit(t *testing.T) {
	s := "small output"
	out, omitted, cut := budgetOutput(s, 1000)
	if out != s || omitted != 0 || cut {
		t.Fatalf("under-limit budget altered output: %q %d %v", out, omitted, cut)
	}
	out, omitted, cut = budgetOutput(s, 0)
	if out != s || omitted != 0 || cut {
		t.Fatalf("zero budget must mean unlimited: %q %d %v", out, omitted, cut)
	}
}

func TestBudgetOutputHandlesTinyLimit(t *testing.T) {
	s := strings.Repeat("y", 10000)
	out, omitted, cut := budgetOutput(s, 64)
	if !cut || len(out) > 64 || omitted <= 0 {
		t.Fatalf("tiny budget not honored: len=%d omitted=%d cut=%v", len(out), omitted, cut)
	}
}

func TestExecStructuredAlwaysPopulatesRequiredFields(t *testing.T) {
	// Every field the outputSchema marks required must be present, including
	// on the nil-result path, or a success result would fail SDK validation.
	required := []string{"execution_state", "exit_code", "shell", "output",
		"duration_ms", "truncated", "discarded_bytes", "output_raw", "safe_to_retry"}
	for _, res := range []*execResult{nil, {State: execNotStarted, Shell: "cmd"}} {
		got := execStructured(res, false, 0)
		for _, key := range required {
			if _, ok := got[key]; !ok {
				t.Fatalf("required field %q missing for %+v", key, res)
			}
		}
	}
}

func TestExecStructuredSeparatesExitCodeFromToolSuccess(t *testing.T) {
	// A failing command (exit 7) is a completed execution, not a tool error:
	// exit_code carries the process result while execution_state stays
	// completed.
	code := 7
	res := &execResult{State: execCompleted, ExitCode: &code, Shell: "pwsh",
		Output: "BEFORE_FAILURE\nEXPECTED_STDERR\n", DurationMs: 1181}
	got := execStructured(res, false, 0)
	if got["execution_state"] != execCompleted {
		t.Fatalf("state = %v, want completed", got["execution_state"])
	}
	if got["exit_code"] != 7 {
		t.Fatalf("exit_code = %v, want 7", got["exit_code"])
	}
	if got["error"] != nil {
		t.Fatalf("a non-zero exit must not be reported as an error: %v", got["error"])
	}
	if got["safe_to_retry"] != false {
		t.Fatalf("a completed command must not claim safe_to_retry")
	}
}

func TestExecStructuredSanitizesByDefaultAndKeepsRawOnRequest(t *testing.T) {
	code := 0
	res := &execResult{State: execCompleted, ExitCode: &code, Shell: "pwsh",
		Output: "\x1b[2J\x1b[H{\"sum\":5,\"ok\":true}\r\n\x1b]0;pwsh\x07", DurationMs: 12}
	clean := execStructured(res, false, 0)
	if clean["output"] != "{\"sum\":5,\"ok\":true}\n" {
		t.Fatalf("sanitized output = %q", clean["output"])
	}
	if clean["output_raw"] != false {
		t.Fatal("output_raw must be false when sanitized")
	}
	raw := execStructured(res, true, 0)
	if raw["output"] != res.Output || raw["output_raw"] != true {
		t.Fatalf("raw request altered output: %q", raw["output"])
	}
}

func TestExecStructuredReportsTruncationFromBothCaps(t *testing.T) {
	code := 0
	res := &execResult{State: execCompleted, ExitCode: &code, Shell: "cmd",
		Output: strings.Repeat("z", 2000), Discarded: 500, DurationMs: 5}
	got := execStructured(res, false, 200)
	if got["truncated"] != true {
		t.Fatal("truncation not reported")
	}
	// Both the internal cap's discard and the response budget's omission count
	// toward the total the caller sees.
	if n, _ := got["discarded_bytes"].(int64); n <= 500 {
		t.Fatalf("discarded_bytes = %v, want internal 500 plus budgeted omission", got["discarded_bytes"])
	}
}

func TestExecStructuredTimeoutIsUnknownOutcomeAndUnsafeRetry(t *testing.T) {
	res := &execResult{State: execTimedOut, Shell: "powershell",
		Output: "BEFORE_TIMEOUT_MARK\n", DurationMs: 1500,
		Error: "exec timed out after 1.5s"}
	got := execStructured(res, false, 0)
	if got["execution_state"] != execTimedOut {
		t.Fatalf("state = %v, want timed_out", got["execution_state"])
	}
	if got["exit_code"] != nil {
		t.Fatalf("exit_code must be null on timeout, got %v", got["exit_code"])
	}
	if got["safe_to_retry"] != false {
		t.Fatal("a timeout leaves the outcome unknown; retry could duplicate side effects")
	}
	if !strings.Contains(got["output"].(string), "BEFORE_TIMEOUT_MARK") {
		t.Fatalf("partial output lost on timeout: %q", got["output"])
	}
}

func TestExecStructuredNotStartedIsSafeRetry(t *testing.T) {
	res := &execResult{State: execNotStarted, Shell: "cmd", Error: "no client matches",
		SafeToRetry: true}
	got := execStructured(res, false, 0)
	if got["safe_to_retry"] != true || got["execution_state"] != execNotStarted {
		t.Fatalf("not-started must be retryable: %+v", got)
	}
	if got["error"] == nil {
		t.Fatal("not-started should carry its reason")
	}
}
