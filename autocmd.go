// The autocmd command is used to execute a command when some source file
// changes.  The source files can be specified as actual path names or using
// shell metacharacters (e.g., *.go).
//
// The typical usage is:
//
//	autocmd --clear '*.go' -- go test
//
// The --edit flag is a special case use, essentially combining a text editor
// with autocmd.  This is useful by scripts that need to open an editor on a
// file and each time the file is written some command (such as publishing)
// should be executed.
//
// The --edit flag turns off verbose and clear and turns on silent and wait.
//
// Normally autocmd immediately executes the specified command.  The --wait
// option causes autocmd wait for the first change to the file before executing
// the command.
//
// Use --more to page the output through the more(1) command.
//
// When a command exits the real (elapsed), user, and system time are displayed.
// If --more is specified then the user and system times may also include
// time spent by the more process.
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
	"sync"
	"syscall"
	"time"

	"github.com/pborman/getopt/v2"
	"github.com/pborman/options"
)

var flags = struct {
	Editor  string        `getopt:"--editor=EDITOR editor to use "`
	Verbose bool          `getopt:"--verbose -v be verbose"`
	Quiet   bool          `getopt:"--silent -s be very very quiet"`
	Timeout time.Duration `getopt:"--timeout=DUR -t set timeout for commands"`
	Clear   bool          `getopt:"--clear -c clear display before executing a command"`
	Edit    bool          `getopt:"--edit use edit mode"`
	Wait    bool          `getopt:"--wait wait for first change"`
	More    bool          `getopt:"--more pipe output through more"`
}{
	Timeout: time.Hour,
	Editor:  "vi",
}

func SameFile(f1, f2 os.FileInfo) bool {
	// We assume that if a file changes modtime then the contents have
	// changed, even though they might not have.  A more complete check
	// would actually look at the contents if the files have the same
	// size but different mod times.  This would require keeping a hash
	// of every file we know about.
	return f1.Size() == f2.Size() && f1.ModTime() == f2.ModTime()
}

func Expand(pattern string) []string {
	pattern = filepath.Clean(pattern)
	var pre, post string
	switch {
	case pattern == "...":
	case strings.HasPrefix(pattern, ".../"):
		post = pattern[4:]
	case strings.HasSuffix(pattern, "/..."):
		pre = pattern[:len(pattern)-4]
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
		if info.IsDir() {
			paths = append(paths, filepath.Join(path, post))
		}
		return nil
	})
	return paths
}

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

func Edit(path string, ch chan error) {
	cmd := exec.Command(flags.Editor, path)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	ch <- cmd.Run()
}

func now() string {
	return time.Now().Format("2006-01-02 15:04:05 MST")
}

func main() {
	var mu sync.Mutex
	clear := func() {}

	var cmd []string
	getopt.SetParameters("PATTERN [...] -- CMD [...]")
	patterns := options.RegisterAndParse(&flags)

	for x, arg := range patterns {
		if arg == "--" {
			cmd = patterns[x+1:]
			patterns = patterns[:x]
		}
	}

	if len(cmd) == 0 || len(patterns) == 0 {
		getopt.PrintUsage(os.Stderr)
		os.Exit(1)
	}
	var acmd, mcmd *exec.Cmd

	var endTime time.Time
	var ech chan error
	finished := make(chan struct{})
	killed := false
	close(finished)

	last := map[string]os.FileInfo{}

	if flags.Edit {
		flags.Wait = true
		flags.Verbose = false
		flags.More = false
		flags.Clear = false
		flags.Quiet = true

		files, err := MultiGlob(patterns)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if len(files) != 1 {
			fmt.Fprint(os.Stderr, "--edit requires exactly one file to watch\n")
			os.Exit(1)
		}
		for pattern := range files {
			patterns = []string{pattern}
			ech = make(chan error, 1)
			go Edit(pattern, ech)
		}
	}

	printf := fmt.Printf
	vprintf := func(f string, v ...interface{}) {}
	vflush := func() {}

	if flags.Verbose {
		var vbuf bytes.Buffer
		vprintf = func(f string, v ...interface{}) {
			fmt.Fprintf(&vbuf, f, v...)
		}
		vflush = func() {
			io.Copy(os.Stdout, &vbuf)
			vbuf.Reset()
		}
	}
	if flags.Quiet {
		printf = func(f string, v ...interface{}) (int, error) { return 0, nil }
	}

	if flags.Clear {
		clear = func() {
			os.Stdout.Write([]byte("\033[H\033[2J\033[3J"))
		}
	}

	done := false
	for {
		files, err := MultiGlob(patterns)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		same := true
		for path, f1 := range files {
			f2, ok := last[path]
			delete(last, path)
			if !ok || !SameFile(f1, f2) {
				same = false
				if !flags.Verbose {
					break
				}
				if ok {
					vprintf("* %s\n", path)
				} else {
					vprintf("+ %s\n", path)
				}
			} else {
				vprintf("= %s\n", path)
			}
		}
		if len(last) != 0 {
			for path := range last {
				vprintf("- %s\n", path)
			}
			same = false
		}
		last = files
		if flags.Wait {
			flags.Wait = false
			time.Sleep(time.Second / 2)
			continue
		}
		if same {
			if done {
				return
			}
			select {
			case err := <-ech:
				if err != nil {
					fmt.Fprintln(os.Stderr, err)
				}
				done = true
				// Do one final check as the editor may have changed the file.
				continue
			case <-finished:
			default:
				if time.Now().Before(endTime) {
					// Not time to kill it
					break
				}
				if killed {
					// Maybe display something?
					break
				}
				// The process has been started
				printf("Killing runaway\n")
				acmd.Process.Kill()
				if mcmd != nil {
					mcmd.Process.Kill()
				}
			}
			time.Sleep(time.Second / 2)
			continue
		}
		if acmd != nil && acmd.Process != nil {
			mu.Lock()
			cmd := acmd
			acmd = nil
			cmd2 := mcmd
			mcmd = nil
			mu.Unlock()
			if cmd != nil {
				printf("%s Killing old command\n", now())
				cmd.Process.Kill()
				printf("%s Waiting for death...\n", now())
				cmd.Wait()
			}
			if cmd2 != nil {
				printf("%s Killing old pager\n", now())
				cmd2.Process.Kill()
				printf("%s Waiting for pager's death...\n", now())
				cmd2.Wait()
			}
		}
		clear()
		vflush()
		printf("%s Starting %s\n", now(), cmd)
		mu.Lock()
		acmd = exec.Command(cmd[0], cmd[1:]...)
		if flags.More {
			mcmd = exec.Command("more", "--dumb")
		}
		mu.Unlock()
		if mcmd == nil {
			acmd.Stdout = os.Stdout
			acmd.Stderr = os.Stderr
		} else {
			r, w, err := os.Pipe()
			if err != nil {
				printf("%v\n", err)
				continue
			}
			mcmd.Stdin = r
			mcmd.Stdout = os.Stdout
			mcmd.Stderr = os.Stderr
			acmd.Stdout = w
			acmd.Stderr = w
		}
		var rusageStart, rusageEnd syscall.Rusage
		syscall.Getrusage(syscall.RUSAGE_CHILDREN, &rusageStart)
		startedRunning := time.Now()
		if err := acmd.Start(); err != nil {
			printf("%v\n", err)
			continue
		}
		if mcmd != nil {
			if err := mcmd.Start(); err != nil {
				printf("%v\n", err)
				acmd.Process.Kill()
			}
		}
		finished = make(chan struct{})
		f := finished
		endTime = time.Now().Add(flags.Timeout)
		var wg sync.WaitGroup
		var cerr, merr error
		wg.Add(1)
		go func() {
			cerr = acmd.Wait()
			syscall.Getrusage(syscall.RUSAGE_CHILDREN, &rusageEnd)
			if mcmd != nil {
				acmd.Stdout.(io.WriteCloser).Close()
			}
			wg.Done()
		}()
		if mcmd != nil {
			wg.Add(1)
			go func() {
				merr = mcmd.Wait()
				wg.Done()
			}()
		}
		go func() {
			wg.Wait()
			user := time.Duration(rusageEnd.Utime.Nano() - rusageStart.Utime.Nano()).Round(time.Millisecond)
			sys := time.Duration(rusageEnd.Stime.Nano() - rusageStart.Stime.Nano()).Round(time.Millisecond)
			real := time.Now().Sub(startedRunning).Round(time.Millisecond)
			if cerr != nil {
				printf("Command died with %v ", cerr)
			} else {
				printf("Command exited ")
			}
			fmt.Printf("(real: %v, user: %v, sys: %v)\n", real, user, sys)
			if merr != nil {
				printf("More died with %v\n", merr)
			}
			close(f)
		}()
	}
}
