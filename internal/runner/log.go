// Writes bounded rotating logs for supervised processes.
package runner

import (
	"fmt"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

const logSegmentSize = 10 * 1024 * 1024

type rotatingLog struct {
	mu   sync.Mutex
	path string
	file *os.File
	size int64
}

func openLog(path string) (*rotatingLog, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm() != 0600 {
		f.Close()
		return nil, fmt.Errorf("unsafe log file %s", path)
	}
	return &rotatingLog{path: path, file: f, size: st.Size()}, nil
}
func (l *rotatingLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	total := 0
	for len(p) > 0 {
		if l.size >= logSegmentSize {
			if err := l.rotate(); err != nil {
				return total, err
			}
		}
		n := len(p)
		if int64(n) > logSegmentSize-l.size {
			n = int(logSegmentSize - l.size)
		}
		written, err := l.file.Write(p[:n])
		total += written
		l.size += int64(written)
		p = p[written:]
		if err != nil {
			return total, err
		}
		if written != n {
			return total, fmt.Errorf("short log write")
		}
	}
	return total, nil
}
func (l *rotatingLog) rotate() error {
	if err := l.file.Close(); err != nil {
		return err
	}
	if err := os.Remove(l.path + ".2"); err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, n := range []int{1, 0} {
		old := l.path
		if n > 0 {
			old += fmt.Sprintf(".%d", n)
		}
		if err := os.Rename(old, fmt.Sprintf("%s.%d", l.path, n+1)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	next, err := openLog(l.path)
	if err != nil {
		return err
	}
	l.file = next.file
	l.size = next.size
	return nil
}
func (l *rotatingLog) Close() error { l.mu.Lock(); defer l.mu.Unlock(); return l.file.Close() }

type logSink struct {
	log    *rotatingLog
	errors chan error
}

func newLogSink(log *rotatingLog) *logSink { return &logSink{log: log, errors: make(chan error, 1)} }
func (s *logSink) Write(p []byte) (int, error) {
	n, err := s.log.Write(p)
	if err != nil {
		select {
		case s.errors <- err:
		default:
		}
	}
	return n, err
}
