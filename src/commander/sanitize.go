// Package commander: terminal output sanitization for MCP consumers.
//
// Every command currently runs through a PTY, so its output carries terminal
// control sequences (screen clears, cursor moves, window titles) that consume
// an AI's context budget without adding information. Cleaning is a
// commander-side presentation choice: the relay and the client are untouched,
// and raw bytes stay available to callers that ask for them.
package commander

import (
	"strings"
	"unicode/utf8"
)

// sanitizeResult is cleaned text plus any trailing bytes that cannot be
// emitted yet. Incremental readers must re-feed that tail with the next chunk
// instead of dropping or emitting it.
type sanitizeResult struct {
	text string
	tail []byte
}

// sanitizeTerminal strips terminal control sequences and normalizes line
// endings to \n. It preserves UTF-8: sequences are only ever split at ASCII
// control bytes, never inside a multi-byte character.
//
// Removals: CSI/OSC/DCS/SOS/PM/APC sequences, other two-byte escapes, and
// stray C0 controls plus DEL. Newlines and tabs survive; \r\n and lone \r both
// become \n, so progress bars that overwrite a line expand into lines.
//
// A trailing incomplete escape sequence, or an incomplete multi-byte UTF-8
// character cut by the network chunk boundary, is returned in tail rather than
// emitted: half a character marshalled to JSON would surface as U+FFFD and
// silently corrupt the text an AI reads.
func sanitizeTerminal(p []byte) sanitizeResult {
	var b strings.Builder
	b.Grow(len(p))
	for i := 0; i < len(p); {
		c := p[i]
		switch {
		case c == 0x1b: // ESC
			n, ok := skipEscape(p[i:])
			if !ok {
				// Ends mid-sequence: hand the remainder back to the caller.
				return sanitizeResult{text: b.String(), tail: cloneBytes(p[i:])}
			}
			i += n
		case c == '\r':
			if i+1 < len(p) && p[i+1] == '\n' {
				i += 2
			} else {
				i++
			}
			b.WriteByte('\n')
		case c == '\n' || c == '\t':
			b.WriteByte(c)
			i++
		case c < 0x20 || c == 0x7f:
			i++ // drop other C0 controls and DEL
		case c >= utf8.RuneSelf:
			r, size := utf8.DecodeRune(p[i:])
			if r == utf8.RuneError && size <= 1 {
				// Either a genuinely invalid byte or a character split by the
				// chunk boundary. If more bytes could still complete it, carry
				// them; otherwise drop the bad byte rather than emit U+FFFD.
				if incomplete := incompleteUTF8Len(p[i:]); incomplete > 0 {
					return sanitizeResult{text: b.String(), tail: cloneBytes(p[i:])}
				}
				i++
				continue
			}
			b.Write(p[i : i+size])
			i += size
		default:
			b.WriteByte(c)
			i++
		}
	}
	return sanitizeResult{text: b.String()}
}

// incompleteUTF8Len reports how many trailing bytes at the start of p form the
// prefix of a longer multi-byte character, or 0 when they do not.
func incompleteUTF8Len(p []byte) int {
	if len(p) == 0 {
		return 0
	}
	// Look back at most 3 bytes: UTF-8 sequences are at most 4 bytes long.
	for back := 1; back <= 3 && back <= len(p); back++ {
		c := p[len(p)-back]
		if c&0xC0 != 0xC0 {
			continue // continuation byte, keep looking for its lead
		}
		want := 0
		switch {
		case c&0xE0 == 0xC0:
			want = 2
		case c&0xF0 == 0xE0:
			want = 3
		case c&0xF8 == 0xF0:
			want = 4
		default:
			return 0 // invalid lead byte
		}
		if back < want {
			return back // lead byte present but the character is cut short
		}
		return 0
	}
	return 0
}

func cloneBytes(p []byte) []byte {
	out := make([]byte, len(p))
	copy(out, p)
	return out
}

// skipEscape returns the length of the complete escape sequence at the start
// of p. ok is false when p ends in the middle of one, which the caller must
// treat as "need more bytes" rather than as malformed input.
func skipEscape(p []byte) (int, bool) {
	if len(p) < 2 {
		return 0, false
	}
	switch p[1] {
	case '[': // CSI: parameter/intermediate bytes then a final byte 0x40-0x7e
		for j := 2; j < len(p); j++ {
			c := p[j]
			if c >= 0x40 && c <= 0x7e {
				return j + 1, true
			}
			if c < 0x20 || c > 0x3f {
				return j + 1, true // malformed: consume through here and resync
			}
		}
		return 0, false
	case ']', 'P', 'X', '^', '_': // OSC/DCS/SOS/PM/APC: terminated by BEL or ST
		for j := 2; j < len(p); j++ {
			switch p[j] {
			case 0x07:
				return j + 1, true
			case 0x1b:
				if j+1 >= len(p) {
					return 0, false // cannot yet tell ST from a new sequence
				}
				if p[j+1] == '\\' {
					return j + 2, true
				}
				return j, true // a new sequence starts here; end before it
			}
		}
		return 0, false
	default:
		// ESC + intermediate bytes (0x20-0x2f)* + final byte (0x30-0x7e),
		// covering charset designators such as ESC ( B.
		j := 1
		for j < len(p) && p[j] >= 0x20 && p[j] <= 0x2f {
			j++
		}
		if j >= len(p) {
			return 0, false
		}
		return j + 1, true
	}
}

// cleanOutput sanitizes a one-shot payload. A trailing incomplete sequence is
// discarded rather than carried, because there is no next chunk to pair it
// with; that can only happen when a stream is cut mid-sequence.
func cleanOutput(s string) string {
	return sanitizeTerminal([]byte(s)).text
}

// budgetOutput applies an AI response budget that is separate from the
// internal memory cap. It keeps the head and the tail, because command output
// usually starts with the interesting part and ends with the error, and
// reports exactly how much was omitted so the caller can ask for more.
//
// limit <= 0 means no budget. The returned string never exceeds limit bytes:
// the omission marker is charged against the budget, not appended to it. Cut
// points fall on UTF-8 character boundaries so the result stays valid text.
// Truncation is lossy for this response only; the caller recovers the rest by
// re-running with a larger limit.
func budgetOutput(s string, limit int) (string, int64, bool) {
	if limit <= 0 || len(s) <= limit {
		return s, 0, false
	}
	omitted := int64(len(s) - limit)
	// A budget too small for both content and a marker keeps the head only.
	if limit < omitMarkerReserve {
		return runeSafeCut(s, limit), omitted, true
	}
	// Reserve room for the marker, then split the remainder 2:1 toward the
	// head. omitted is derived from the bytes actually kept after boundary
	// adjustment, so the reported gap matches the text exactly.
	usable := limit - omitMarkerReserve
	tail := runeSafeTailLen(s, usable/3)
	head := runeSafeCutLen(s, usable-tail)
	omitted = int64(len(s) - head - tail)
	out := s[:head] + fmtOmitted(omitted) + s[len(s)-tail:]
	// Boundary adjustment can still leave the marker pushing past the budget
	// when the omitted count is unusually wide; shrink the head until it fits.
	for len(out) > limit && head > 0 {
		head = runeSafeCutLen(s, head-(len(out)-limit))
		tail = runeSafeTailLen(s, tail)
		if head == 0 && tail == 0 {
			return "", int64(len(s)), true
		}
		omitted = int64(len(s) - head - tail)
		out = s[:head] + fmtOmitted(omitted) + s[len(s)-tail:]
	}
	return out, omitted, true
}

// omitMarkerReserve is the marker's size budget, covering its fixed text plus
// a wide omitted count.
const omitMarkerReserve = 96

// runeSafeCut truncates s to at most n bytes without splitting a character.
func runeSafeCut(s string, n int) string {
	return s[:runeSafeCutLen(s, n)]
}

// runeSafeCutLen shrinks n until s[:n] ends on a character boundary.
func runeSafeCutLen(s string, n int) int {
	if n <= 0 {
		return 0
	}
	if n >= len(s) {
		return len(s)
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return n
}

// runeSafeTailLen shrinks n until s[len(s)-n:] starts on a character boundary.
func runeSafeTailLen(s string, n int) int {
	if n <= 0 {
		return 0
	}
	if n >= len(s) {
		return len(s)
	}
	start := len(s) - n
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return len(s) - start
}

func fmtOmitted(n int64) string {
	return "\n[... " + itoa(n) + " bytes omitted; raise max_output_bytes to see more ...]\n"
}

// itoa avoids pulling strconv into this file's hot path.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
