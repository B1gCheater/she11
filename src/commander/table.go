package commander

import (
	"os"
	"strings"
	"unicode"

	"golang.org/x/term"
)

// stdoutWidth reports the terminal width, or 0 when stdout is not a terminal.
// Piped or redirected output keeps its natural width instead of being cut to a
// guessed size.
func stdoutWidth() int {
	if width, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && width > 0 {
		return width
	}
	return 0
}

// displayWidth counts the terminal cells a string occupies. Wide East Asian
// runes count as two and combining marks as zero, so a table mixing ASCII and
// CJK still lines up.
func displayWidth(text string) int {
	width := 0
	for _, r := range text {
		width += runeWidth(r)
	}
	return width
}

func runeWidth(r rune) int {
	if r == 0 || unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || r == 0x200B {
		return 0
	}
	if isWideRune(r) {
		return 2
	}
	return 1
}

func isWideRune(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x115F, // Hangul Jamo
		r >= 0x2E80 && r <= 0x303E, // CJK radicals, Kangxi, punctuation
		r >= 0x3041 && r <= 0x33FF, // kana, CJK compatibility
		r >= 0x3400 && r <= 0x4DBF, // CJK extension A
		r >= 0x4E00 && r <= 0x9FFF, // CJK unified
		r >= 0xA000 && r <= 0xA4CF, // Yi
		r >= 0xA960 && r <= 0xA97F, // Hangul Jamo extended A
		r >= 0xAC00 && r <= 0xD7A3, // Hangul syllables
		r >= 0xF900 && r <= 0xFAFF, // CJK compatibility ideographs
		r >= 0xFE10 && r <= 0xFE19,
		r >= 0xFE30 && r <= 0xFE6F,
		r >= 0xFF00 && r <= 0xFF60, // fullwidth forms
		r >= 0xFFE0 && r <= 0xFFE6,
		r >= 0x1F300 && r <= 0x1F64F, // emoji
		r >= 0x1F900 && r <= 0x1F9FF,
		r >= 0x20000 && r <= 0x3FFFD: // CJK extension B and beyond
		return true
	}
	return false
}

// fitCell pads or truncates text to exactly width cells. Control characters
// become spaces so one bad value cannot break the layout, and a truncated value
// ends with an ellipsis so it is never mistaken for the complete value.
func fitCell(text string, width int) string {
	if width <= 0 {
		return ""
	}
	var b strings.Builder
	used := 0
	clipped := false
	for _, r := range text {
		if r < 0x20 || r == 0x7F {
			r = ' '
		}
		cells := runeWidth(r)
		if used+cells > width {
			clipped = true
			break
		}
		b.WriteRune(r)
		used += cells
	}
	if !clipped {
		if used == width {
			return b.String()
		}
		b.WriteString(strings.Repeat(" ", width-used))
		return b.String()
	}
	// Make room for the ellipsis by dropping the tail of what was accepted.
	out := []rune(b.String())
	for used >= width && len(out) > 0 {
		used -= runeWidth(out[len(out)-1])
		out = out[:len(out)-1]
	}
	text = string(out)
	pad := width - 1 - displayWidth(text)
	if pad < 0 {
		pad = 0
	}
	return text + "\u2026" + strings.Repeat(" ", pad)
}

// renderTable draws columns at the width of their widest cell, bounded by caps
// (0 means unbounded) and, when termWidth is positive, by the terminal width.
// Columns are separated by " | " and the header is underlined with "-+-"
// junctions. truncated reports that at least one value was shortened.
func renderTable(headers []string, rows [][]string, caps []int, termWidth int) (string, bool) {
	columns := len(headers)
	if columns == 0 {
		return "", false
	}
	widths := make([]int, columns)
	minimum := make([]int, columns)
	for i, header := range headers {
		widths[i] = displayWidth(header)
		minimum[i] = widths[i]
	}
	for _, row := range rows {
		for i := 0; i < columns && i < len(row); i++ {
			if w := displayWidth(row[i]); w > widths[i] {
				widths[i] = w
			}
		}
	}
	for i := range widths {
		if i < len(caps) && caps[i] > 0 && widths[i] > caps[i] {
			widths[i] = caps[i]
		}
		if widths[i] < minimum[i] {
			widths[i] = minimum[i]
		}
	}
	if termWidth > 0 {
		shrinkToFit(widths, minimum, termWidth)
	}

	truncated := false
	for _, row := range rows {
		for i := 0; i < columns && i < len(row); i++ {
			if displayWidth(row[i]) > widths[i] {
				truncated = true
				break
			}
		}
		if truncated {
			break
		}
	}

	var b strings.Builder
	writeRow := func(cells []string) {
		for i := 0; i < columns; i++ {
			if i > 0 {
				b.WriteString(" | ")
			}
			cell := ""
			if i < len(cells) {
				cell = cells[i]
			}
			b.WriteString(fitCell(cell, widths[i]))
		}
		b.WriteString("\n")
	}
	writeRow(headers)
	for i := 0; i < columns; i++ {
		if i > 0 {
			b.WriteString("-+-")
		}
		b.WriteString(strings.Repeat("-", widths[i]))
	}
	b.WriteString("\n")
	for _, row := range rows {
		writeRow(row)
	}
	return b.String(), truncated
}

// shrinkComfort is the width above which a column is considered long-form
// text (a user name, a shell list) and is trimmed first. Short structured
// fields such as OS/ARCH or VIA keep their full values whenever possible.
const shrinkComfort = 16

// shrinkToFit narrows columns until the table fits the terminal, never below
// the header width. Long columns absorb the loss first; only when that is not
// enough does it touch short columns, and a terminal too narrow for the
// headers is left overflowing rather than mangling them.
func shrinkToFit(widths, minimum []int, termWidth int) {
	total := func() int {
		sum := 3 * (len(widths) - 1)
		for _, w := range widths {
			sum += w
		}
		return sum
	}
	for total() > termWidth {
		widest := -1
		for i, w := range widths {
			if w > shrinkComfort && (widest < 0 || w > widths[widest]) {
				widest = i
			}
		}
		if widest < 0 {
			break
		}
		widths[widest]--
	}
	for total() > termWidth {
		widest := -1
		for i, w := range widths {
			if w > minimum[i] && (widest < 0 || w > widths[widest]) {
				widest = i
			}
		}
		if widest < 0 {
			return
		}
		widths[widest]--
	}
}
