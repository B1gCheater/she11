package server

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Logger writes connection logs and per-session shell dumps.
// folder=="" disables everything.
type Logger struct {
	folder string
	level  string

	mu       sync.Mutex
	connLogs map[string]*bufio.Writer // yyyy-mm-dd -> writer
	connFile *os.File
}

// NewLogger creates the logger (no file I/O yet).
func NewLogger(folder, level string) *Logger {
	return &Logger{folder: folder, level: level, connLogs: map[string]*bufio.Writer{}}
}

func (l *Logger) connLine(line string) {
	if l.folder == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	day := time.Now().Format("2006-01-02")
	w := l.connLogs[day]
	if w == nil {
		dir := filepath.Join(l.folder, "connection")
		_ = os.MkdirAll(dir, 0755)
		f, err := os.OpenFile(filepath.Join(dir, day+".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			return
		}
		w = bufio.NewWriter(f)
		l.connLogs[day] = w
	}
	_, _ = w.WriteString(time.Now().Format("15:04:05.000") + " " + line + "\n")
	_ = w.Flush()
}

// Printf logs an info line (respecting level).
func (l *Logger) Printf(format string, a ...interface{}) {
	if l.level == "error" {
		return
	}
	// server always has console: also print to stdout
	fmt.Printf("[server] "+format+"\n", a...)
	l.connLine(fmt.Sprintf(format, a...))
}

// DumpWriter writes relayed bytes of one session to shell_dump.
type DumpWriter struct {
	mu     sync.Mutex
	logger *Logger
	file   *os.File
	w      *bufio.Writer
	dir    string // "in" | "out"
	label  string
}

// SessionDump opens the dump file for a session.
func (l *Logger) SessionDump(shell, sid string) (*DumpWriter, error) {
	if l.folder == "" {
		return &DumpWriter{logger: l}, nil
	}
	dir := filepath.Join(l.folder, "shell_dump")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	name := fmt.Sprintf("%s_%s.log", shell, sid)
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	return &DumpWriter{logger: l, file: f, w: bufio.NewWriter(f)}, nil
}

// dumpWriter adapts a DumpWriter to io.Writer for io.MultiWriter use.
type dumpWriter struct {
	d   *DumpWriter
	dir string
}

func (w dumpWriter) Write(p []byte) (int, error) {
	if w.d != nil {
		_ = w.d.WriteLabeled(w.dir, p)
	}
	return len(p), nil
}

// Write records a chunk as "<ts> <dir> <b64>\n".
func (d *DumpWriter) WriteLabeled(dir string, p []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dir = dir
	return d.writeChunk(p)
}

func (d *DumpWriter) writeChunk(p []byte) error {
	if d.w == nil || len(p) == 0 {
		return nil
	}
	line := fmt.Sprintf("%s %s %s\n", time.Now().Format("15:04:05.000"), d.dir, base64.StdEncoding.EncodeToString(p))
	_, err := d.w.WriteString(line)
	if err == nil {
		err = d.w.Flush()
	}
	return err
}

// Close finalizes the dump file.
func (d *DumpWriter) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.w != nil {
		_ = d.w.Flush()
	}
	if d.file != nil {
		_ = d.file.Close()
	}
	d.w = nil
	d.file = nil
}
