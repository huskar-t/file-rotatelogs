// package rotatelogs is a port of File-RotateLogs from Perl
// (https://metacpan.org/release/File-RotateLogs), and it allows
// you to automatically rotate output files when you write to them
// according to the filename pattern that you can specify.
package rotatelogs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"bitbucket.org/tebeka/strftime"
)

func (c clockFn) Now() time.Time {
	return c()
}

func (o OptionFn) Configure(rl *RotateLogs) error {
	return o(rl)
}

// WithClock creates a new Option that sets a clock
// that the RotateLogs object will use to determine
// the current time. 
//
// By default the local time is used. If you would rather
// use UTC, create an object that returns the time in UTC
// instead of the local time zone.
func WithClock(c Clock) Option {
	return OptionFn(func(rl *RotateLogs) error {
		rl.clock = c
		return nil
	})
}

// WithLinkName creates a new Option that sets the
// symbolic link name that gets linked to the current
// file name being used.
func WithLinkName(s string) Option {
	return OptionFn(func(rl *RotateLogs) error {
		rl.linkName = s
		return nil
	})
}

// WithMaxAge creates a new Option that sets the
// max age of a log file before it gets purged from
// the file system.
func WithMaxAge(d time.Duration) Option {
	return OptionFn(func(rl *RotateLogs) error {
		rl.maxAge = d
		return nil
	})
}

// WithRotationTime creates a new Option that sets the
// time between rotation.
func WithRotationTime(d time.Duration) Option {
	return OptionFn(func(rl *RotateLogs) error {
		rl.rotationTime = d
		return nil
	})
}

// New creates a new RotateLogs object. A log filename pattern
// must be passed. Optional `Option` parameters may be passed
func New(pattern string, options ...Option) *RotateLogs {
	globPattern := pattern
	for _, re := range patternConversionRegexps {
		globPattern = re.ReplaceAllString(globPattern, "*")
	}

	var rl RotateLogs
	rl.clock = clockFn(time.Now)
	rl.globPattern = globPattern
	rl.pattern = pattern
	rl.rotationTime = 24 * time.Hour
	for _, opt := range options {
		opt.Configure(&rl)
	}

	return &rl
}

func (rl *RotateLogs) genFilename() (string, error) {
	now := rl.clock.Now()
	diff := time.Duration(now.UnixNano()) % rl.rotationTime
	t := now.Add(time.Duration(-1 * diff))
	str, err := strftime.Format(rl.pattern, t)
	if err != nil {
		return "", err
	}
	return str, err
}

// Write satisfies the io.Writer interface. It writes to the
// appropriate file handle that is currently being used.
// If we have reached rotation time, the target file gets
// automatically rotated, and also purged if necessary.
func (rl *RotateLogs) Write(p []byte) (n int, err error) {
	// Guard against concurrent writes
	rl.mutex.Lock()
	defer rl.mutex.Unlock()

	// This filename contains the name of the "NEW" filename
	// to log to, which may be newer than rl.currentFilename

	filename, err := rl.genFilename()
	if err != nil {
		return 0, err
	}

	var out *os.File
	if filename == rl.curFn { // Match!
		out = rl.outFh // use old one
	}

	var isNew bool

	if out == nil {
		isNew = true

		_, err := os.Stat(filename)
		if err == nil {
			if rl.linkName != "" {
				_, err = os.Lstat(rl.linkName)
				if err == nil {
					isNew = false
				}
			}
		}

		fh, err := os.OpenFile(filename, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			return 0, fmt.Errorf("error: Failed to open file %s: %s", rl.pattern, err)
		}

		out = fh
		if isNew {
			rl.rotate(filename)
		}
	}

	n, err = out.Write(p)

	if rl.outFh == nil {
		rl.outFh = out
	} else if isNew {
		rl.outFh.Close()
		rl.outFh = out
	}
	rl.curFn = filename

	return n, err
}

// CurrentFileName returns the current file name that
// the RotateLogs object is writing to
func (rl *RotateLogs) CurrentFileName() string {
	rl.mutex.RLock()
	defer rl.mutex.RUnlock()
	return rl.curFn
}

var patternConversionRegexps = []*regexp.Regexp{
	regexp.MustCompile(`%[%+A-Za-z]`),
	regexp.MustCompile(`\*+`),
}

type cleanupGuard struct {
	enable bool
	fn     func()
	mutex  sync.Mutex
}

func (g *cleanupGuard) Enable() {
	g.mutex.Lock()
	defer g.mutex.Unlock()
	g.enable = true
}
func (g *cleanupGuard) Run() {
	g.fn()
}

func (rl *RotateLogs) rotate(filename string) error {
	lockfn := fmt.Sprintf("%s_lock", filename)

	fh, err := os.OpenFile(lockfn, os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		// Can't lock, just return
		return err
	}

	var guard cleanupGuard
	guard.fn = func() {
		fh.Close()
		os.Remove(lockfn)
	}
	defer guard.Run()

	if rl.linkName != "" {
		tmpLinkName := fmt.Sprintf("%s_symlink", filename)
		err = os.Symlink(filename, tmpLinkName)
		if err != nil {
			return err
		}

		err = os.Rename(tmpLinkName, rl.linkName)
		if err != nil {
			return err
		}
	}

	if rl.maxAge <= 0 {
		return errors.New("maxAge not set, not rotating")
	}

	matches, err := filepath.Glob(rl.globPattern)
	if err != nil {
		return err
	}

	cutoff := rl.clock.Now().Add(-1 * rl.maxAge)
	var toUnlink []string
	for _, path := range matches {
		// Ignore lock files
		if strings.HasSuffix(path, "_lock") || strings.HasSuffix(path, "_symlink") {
			continue
		}

		fi, err := os.Stat(path)
		if err != nil {
			continue
		}

		if fi.ModTime().After(cutoff) {
			continue
		}
		toUnlink = append(toUnlink, path)
	}

	if len(toUnlink) <= 0 {
		return errors.New("nothing to unlink")
	}

	guard.Enable()
	go func() {
		// unlink files on a separate goroutine
		for _, path := range toUnlink {
			os.Remove(path)
		}
	}()

	return nil
}

// Close satisfies the io.Closer interface. You must
// call this method if you performed any writes to
// the object.
func (rl *RotateLogs) Close() error {
	rl.mutex.Lock()
	defer rl.mutex.Unlock()

	if rl.outFh == nil {
		return nil
	}

	rl.outFh.Close()
	rl.outFh = nil
	return nil
}
