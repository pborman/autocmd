// The autocmd command is used to execute a command when some source file
// changes.  The source files can be specified as actual path names or using
// shell metacharacters (e.g., *.go).
//
// The typical usage is:
//
//	autocmd --clear '*.go' -- go test
//
// Normally autocmd immediately executes the specified command.  The --wait
// option causes autocmd wait for the first change to the file before executing
// the command.
//
// The --go flag is a short cut to specify --clear and all .go files from the
// current directory on down.  The --go flag implies --, typical usage;
//
//	autocmd --go go test
//
// Multiple sets of commands can be set to run by separating them with ---.
// For example:
//
//	autocmd grammer.y -- goyacc -o grammar.go grammar.y \
//		--- .../*.go go build
//
// This will cause autocmd to run goyacc if grammer.y changes and go build
// if any .go file changes.  If grammar.y changes then grammer.go will change
// which will trigger the go build.
package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pborman/getopt/v2"
	"github.com/pborman/options"
)

var flags = struct {
	Git       bool          `getopt:"--git do not ignore .git directories exapnded by ..."`
	Go        bool          `getopt:"--go shorthand for '--clear ./.../*.go --'"`
	Verbose   bool          `getopt:"--verbose -v be verbose"`
	Quiet     bool          `getopt:"--silent -s be very very quiet"`
	Timeout   time.Duration `getopt:"--timeout=DUR -t set timeout for commands"`
	Clear     bool          `getopt:"--clear -c clear display before executing a command"`
	Wait      bool          `getopt:"--wait wait for first change"`
	Frequency time.Duration `getopt:"--frequency=DUR -f set time to delay between checks"`
}{
	Timeout:   time.Hour,
	Frequency: time.Second / 2,
}

// SameFile returns true if f1 and f2 appear to be the same file.
func SameFile(f1, f2 os.FileInfo) bool {
	// We assume that if a file changes modtime then the contents have
	// changed, even though they might not have.  A more complete check
	// would actually look at the contents if the files have the same
	// size but different mod times.  This would require keeping a hash
	// of every file we know about.
	return f1.Size() == f2.Size() && f1.ModTime() == f2.ModTime()
}

// Expand expands up to 1 occurrence of "..." in pattern and returns
// all the flies/directories that match the expansion.
func Expand(pattern string) []string {
	pattern = filepath.Clean(pattern)
	var pre, post string
	switch {
	case pattern == "...":
		post = "*"
	case strings.HasPrefix(pattern, ".../"):
		post = pattern[4:]
	case strings.HasSuffix(pattern, "/..."):
		pre = pattern[:len(pattern)-4]
		post = "*"
	default:
		x := strings.Index(pattern, "/.../")
		if x < 0 {
			return []string{pattern}
		}
		pre = pattern[:x]
		post = pattern[x+4:]
	}
	if pre == "" {
		pre = "."
	}
	var paths []string
	filepath.Walk(pre, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info == nil || !info.IsDir() {
			return nil
		}
		if !flags.Git && filepath.Base(path) == ".git" {
			return filepath.SkipDir
		}
		paths = append(paths, filepath.Join(path, post))
		return nil
	})
	return paths
}

// MultiBlob returns a map of pathnames to os.FileInfo that match one of the
// provided patterns.  Each pattern is first expanded by Expand and then
// filepath.Glob is applied to each expanded pattern.  An error is returned
// if filepath.Glob returns an error.
func MultiGlob(patterns []string) (map[string]os.FileInfo, error) {
	var matches []string
	for _, p := range patterns {
		for _, p := range Expand(p) {
			m, err := filepath.Glob(p)
			if err != nil {
				return nil, err
			}
			matches = append(matches, m...)
		}
	}
	sort.Strings(matches)
	f := make(map[string]os.FileInfo, len(matches))
	for _, path := range matches {
		if fi, err := os.Stat(path); err == nil {
			f[path] = fi
		}
	}
	return f, nil
}

var now = time.Now

type set struct {
	command  []string
	patterns []string
	seen     map[string]os.FileInfo
}

func newSet(args []string) *set {
	var s set
	s.seen = map[string]os.FileInfo{}
	for x, arg := range args {
		if arg == "--" {
			s.command = args[x+1:]
			s.patterns = args[:x]
			break
		}
	}

	if len(s.command) == 0 || len(s.patterns) == 0 {
		getopt.PrintUsage(os.Stderr)
		os.Exit(1)
	}
	return &s
}

var (
	printf   = fmt.Printf
	clear    = func() {}
	vprintf  = func(f string, v ...interface{}) {}
	vprintf2 = func(f string, v ...interface{}) {}
	vflush   = func() {} // write out the contents of the vprintf buffer
	vadd     = func() {} // append the vprintf2 buffer to the vprintf buffer
	vclear   = func() {} // clear the vprintf2 buffer
)

func main() {
	getopt.SetParameters("PATTERN [...] -- CMD [...] [--- CMD [...] ...]")

	var sets []*set

	patterns := options.RegisterAndParse(&flags)
	if len(patterns) == 0 {
		getopt.PrintUsage(os.Stderr)
		os.Exit(1)
	}

	if flags.Go {
		flags.Clear = true
		sets = []*set{{
			command:  patterns,
			patterns: []string{".../*.go"},
			seen:     map[string]os.FileInfo{},
		}}
	} else {
		sets = []*set{
			newSet(patterns),
		}
	}
	for i := 0; i < len(sets); i++ {
		for x, param := range sets[i].command {
			if param == "---" {
				sets = append(sets, newSet(sets[i].command[x+1:]))
				sets[i].command = sets[i].command[:x]
				break
			}
		}
	}

	var cmd *exec.Cmd

	var endTime time.Time

	if flags.Quiet {
		printf = func(f string, v ...interface{}) (int, error) { return 0, nil }
	}

	// Verbose functions.  They only have effect when flags.Versose is on.
	// The vprintf2 buffer is cleared before each pass.  If a pass finds
	// changes then the vprintf2 buffer is writen to the vprintf buffer.

	if flags.Verbose {
		var vbuf bytes.Buffer
		var vbuf2 bytes.Buffer
		vprintf = func(f string, v ...interface{}) {
			fmt.Fprintf(&vbuf, f, v...)
		}
		vprintf2 = func(f string, v ...interface{}) {
			fmt.Fprintf(&vbuf2, f, v...)
		}
		vclear = func() {
			vbuf2.Reset()
		}
		vadd = func() {
			io.Copy(&vbuf, &vbuf2)
		}
		vflush = func() {
			io.Copy(os.Stdout, &vbuf)
			vbuf.Reset()
		}
	}

	if flags.Clear {
		clear = func() {
			os.Stdout.Write([]byte("\033[H\033[2J\033[3J"))
		}
	}

	if flags.Wait {
		for _, s := range sets {
			s.same()
		}
		time.Sleep(flags.Frequency)
	}

	t := time.NewTicker(flags.Frequency)
	finished := make(chan struct{})
	close(finished)

	for tick := range t.C {
		select {
		case <-finished:
		default:
			if tick.After(endTime) {
				printf("Killing runaway\n")
				cmd.Process.Kill()
			}
		}
		for _, s := range sets {
			if s.same() {
				continue
			}
			// A command might still be running.
			if cmd != nil && cmd.Process != nil {
				printf("%s Killing old command\n", now())
				cmd.Process.Kill()
				printf("%s Waiting for death...\n", now())
				cmd.Wait()
			}
			endTime = now().Add(flags.Timeout)
			cmd, finished = s.run()
			break
		}
	}
}

func (s *set) same() bool {
	// Collect all files currently matching our pattern
	files, err := MultiGlob(s.patterns)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// Compare them with what we have seen before.
	// Anything left in Seen has been deleted.
	// Anything not in Seen is new.
	same := true
	vclear()
	for path, f1 := range files {
		// Skip directories
		if f1.IsDir() {
			delete(files, path)
			continue
		}
		f2, ok := s.seen[path]
		delete(s.seen, path)
		if !ok || !SameFile(f1, f2) {
			same = false
			if !flags.Verbose {
				// Once we have seen one difference
				// we can stop checking, unless we are
				// in verbose mode in which case we
				// have to keep checking.
				break
			}
			if ok {
				vprintf2("* %s\n", path)
			} else {
				vprintf2("+ %s\n", path)
			}
		} else {
			vprintf2("= %s\n", path)
		}
	}
	if len(s.seen) != 0 {
		if flags.Verbose {
			for path := range s.seen {
				vprintf2("- %s\n", path)
			}
		}
		same = false
	}
	s.seen = files
	return same
}

func (s *set) run() (*exec.Cmd, chan struct{}) {
	vadd()
	clear()
	vflush()

	// At this point we assume the spawned processes have
	// completed.  We forget about them.

	printf("%s Starting %s\n", now(), s.command)

	cmd := exec.Command(s.command[0], s.command[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	finished := make(chan struct{})
	if err := cmd.Start(); err != nil {
		printf("%v\n", err)
		cmd = nil
		close(finished)
		return nil, finished
	}

	go func(cmd *exec.Cmd, finished chan struct{}) {
		err := cmd.Wait()
		vprintf("command returns %v\n", err)
		if err != nil {
			printf("Command died with %v\n", err)
		} else {
			printf("Command exited ")
		}
		close(finished)
	}(cmd, finished)
	return cmd, finished
}
