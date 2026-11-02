package main

import (
	"fmt"
	"os"
	"sync"
)

const (
	maxLogSize = 10 << 20 // a log file moves aside past this size
	logFiles   = 3        // name, name.1 and name.2
)

// logFile is the log a daemon writes under a service manager that keeps no
// log of its own: appended to, and moved aside at maxLogSize so that only
// logFiles files are ever kept.
type logFile struct {
	mu   sync.Mutex
	path string
	f    *os.File
	size int64
}

func openLog(path string) (*logFile, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &logFile{path: path, f: f, size: st.Size()}, nil
}

// Write writes one log record; the logger writes each record whole, so a
// file never ends mid-record.
func (l *logFile) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil && l.size > 0 && l.size+int64(len(p)) > maxLogSize {
		l.rotate()
	}
	if l.f == nil {
		f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return len(p), nil // still cannot open it: drop rather than fail the daemon
		}
		// Its size, not zero: a rename that failed (a reader holding it on
		// Windows, say) left the old contents in place.
		l.f, l.size = f, 0
		if st, err := f.Stat(); err == nil {
			l.size = st.Size()
		}
	}
	n, err := l.f.Write(p)
	l.size += int64(n)
	return n, err
}

// rotate moves name.1 to name.2 and name to name.1; Write then starts name
// anew. The file is closed first: Windows renames no open file.
func (l *logFile) rotate() {
	l.f.Close()
	for i := logFiles - 1; i > 0; i-- {
		from := l.path
		if i > 1 {
			from = fmt.Sprintf("%s.%d", l.path, i-1)
		}
		os.Rename(from, fmt.Sprintf("%s.%d", l.path, i))
	}
	l.f = nil // Write opens name anew
}

func (l *logFile) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	return l.f.Close()
}
