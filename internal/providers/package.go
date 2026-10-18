// Package providers implements the system-facing side of each resource type.
package providers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/klahr/pose/internal/config"
)

// Output receives the live output of mutating commands (installers, hooks,
// service managers) so the user can follow progress and answer sudo prompts.
var Output io.Writer = os.Stderr

// CommandError reports a failed subprocess along with the tail of its output.
type CommandError struct {
	Command  string
	ExitCode int // -1 if the process did not exit normally
	Stderr   string
	Err      error
}

func (e *CommandError) Error() string {
	msg := fmt.Sprintf("command `%s` failed", e.Command)
	if e.ExitCode >= 0 {
		msg += fmt.Sprintf(" with exit code %d", e.ExitCode)
	} else {
		msg += ": " + e.Err.Error()
	}
	if out := tail(e.Stderr, 20); out != "" {
		msg += "\n    " + strings.ReplaceAll(out, "\n", "\n    ")
	}
	return msg
}

func (e *CommandError) Unwrap() error { return e.Err }

// exitedNonZero reports whether err is a command that ran and returned a failure
// status, as opposed to one that could not be started or was killed. The shell
// reports a missing or non-executable command as 127 or 126; those count as
// "could not be started" too.
func exitedNonZero(err error) bool {
	var ce *CommandError
	return errors.As(err, &ce) && ce.ExitCode > 0 && ce.ExitCode != 126 && ce.ExitCode != 127
}

type result struct {
	stdout, stderr string
}

// run executes argv directly (no shell). When interactive, stdin is attached so
// sudo can prompt, and output is streamed to Output as well as captured.
func run(ctx context.Context, interactive bool, dir string, argv ...string) (result, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.WaitDelay = 5 * time.Second
	var stdout, stderr bytes.Buffer
	if interactive {
		cmd.Stdin = os.Stdin
		cmd.Stdout = io.MultiWriter(Output, &stdout)
		cmd.Stderr = io.MultiWriter(Output, &stderr)
	} else {
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
	}
	err := cmd.Run()
	res := result{stdout: stdout.String(), stderr: stderr.String()}
	if err == nil {
		return res, nil
	}

	display := strings.Join(quoteAll(argv), " ")
	if len(argv) == 3 && argv[0] == shell && argv[1] == "-c" {
		display = argv[2]
	}
	ce := &CommandError{Command: display, ExitCode: -1, Stderr: res.stderr, Err: err}
	if ce.Stderr == "" {
		ce.Stderr = res.stdout
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.Exited() {
		ce.ExitCode = ee.ExitCode()
	}
	return res, ce
}

const shell = "/bin/sh"

func runShell(ctx context.Context, interactive bool, dir, script string) (result, error) {
	return run(ctx, interactive, dir, shell, "-c", script)
}

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9@%+=:,./_-]+$`)

// ShellQuote returns s quoted for POSIX sh; safe words are returned unchanged.
func ShellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if shellSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func quoteAll(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = ShellQuote(a)
	}
	return out
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// PackageManager runs the check/install/remove contract of one manager.
type PackageManager struct {
	Name string
	def  config.PackageManager

	mu     sync.Mutex
	listed map[string]bool // output of the list command, nil until queried
}

func NewPackageManager(name string, def config.PackageManager) *PackageManager {
	return &PackageManager{Name: name, def: def}
}

func (m *PackageManager) render(tmpl string, pkgs []string) string {
	quoted := quoteAll(pkgs)
	return strings.NewReplacer(
		"{packages}", strings.Join(quoted, " "),
		"{package}", quoted[0],
	).Replace(tmpl)
}

// IsInstalled runs the check command: exit 0 means installed, any other exit
// status means not installed. Failing to run the command at all is an error.
func (m *PackageManager) IsInstalled(ctx context.Context, pkg string) (bool, error) {
	if m.def.List != "" {
		set, err := m.list(ctx)
		if err != nil {
			return false, fmt.Errorf("list %s packages: %w", m.Name, err)
		}
		return set[pkg], nil
	}
	_, err := runShell(ctx, false, "", m.render(m.def.Check, []string{pkg}))
	switch {
	case err == nil:
		return true, nil
	case exitedNonZero(err):
		return false, nil
	}
	return false, fmt.Errorf("check %s package %q: %w", m.Name, pkg, err)
}

// list runs the list command once and caches the result until the next mutation.
func (m *PackageManager) list(ctx context.Context) (map[string]bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listed != nil {
		return m.listed, nil
	}
	res, err := runShell(ctx, false, "", m.def.List)
	if err != nil {
		return nil, err
	}
	m.listed = map[string]bool{}
	for _, line := range strings.Split(res.stdout, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			m.listed[line] = true
		}
	}
	return m.listed, nil
}

func (m *PackageManager) Install(ctx context.Context, pkgs []string) error {
	return m.mutate(ctx, m.def.Install, pkgs)
}

func (m *PackageManager) Remove(ctx context.Context, pkgs []string) error {
	return m.mutate(ctx, m.def.Remove, pkgs)
}

// mutate runs tmpl once for all packages if it uses {packages}, else once per package.
func (m *PackageManager) mutate(ctx context.Context, tmpl string, pkgs []string) error {
	if len(pkgs) == 0 {
		return nil
	}
	m.mu.Lock()
	m.listed = nil
	m.mu.Unlock()
	if strings.Contains(tmpl, "{packages}") {
		_, err := runShell(ctx, true, "", m.render(tmpl, pkgs))
		return err
	}
	for _, p := range pkgs {
		if _, err := runShell(ctx, true, "", m.render(tmpl, []string{p})); err != nil {
			return err
		}
	}
	return nil
}
