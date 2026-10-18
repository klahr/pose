package engine

import (
	"fmt"
	"io"
	"os"
	"syscall"
	"unsafe"

	"github.com/klahr/pose/internal/state"
)

const (
	ansiReset  = "\033[0m"
	ansiBold   = "\033[1m"
	ansiDim    = "\033[2m"
	ansiRed    = "\033[31m"
	ansiGreen  = "\033[32m"
	ansiYellow = "\033[33m"
)

type UI struct {
	out   io.Writer
	color bool
}

// NewUI writes to f, using color when f is a terminal, NO_COLOR is unset and
// noColor is false.
func NewUI(f *os.File, noColor bool) *UI {
	color := !noColor && IsTerminal(f) && os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb"
	return &UI{out: f, color: color}
}

// IsTerminal reports whether f is a TTY (not merely a character device like /dev/null).
func IsTerminal(f *os.File) bool {
	var t syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(ioctlGetTermios), uintptr(unsafe.Pointer(&t)))
	return errno == 0
}

func (u *UI) paint(code, s string) string {
	if !u.color {
		return s
	}
	return code + s + ansiReset
}

func (u *UI) Printf(format string, args ...any) { fmt.Fprintf(u.out, format, args...) }

func (u *UI) Info(format string, args ...any) {
	fmt.Fprintf(u.out, "%s %s\n", u.paint(ansiBold, "[+]"), fmt.Sprintf(format, args...))
}

func (u *UI) Warn(format string, args ...any) {
	fmt.Fprintf(u.out, "%s %s\n", u.paint(ansiYellow+ansiBold, "[!]"), fmt.Sprintf(format, args...))
}

func (u *UI) Error(format string, args ...any) {
	fmt.Fprintf(u.out, "%s %s\n", u.paint(ansiRed+ansiBold, "[x]"), fmt.Sprintf(format, args...))
}

func (u *UI) Note(format string, args ...any) {
	fmt.Fprintf(u.out, "      %s\n", u.paint(ansiDim, fmt.Sprintf(format, args...)))
}

func (u *UI) symbol(k Kind) string {
	switch k {
	case Install, Link:
		return u.paint(ansiGreen, "+")
	case Update:
		return u.paint(ansiYellow, "~")
	case Prune:
		return u.paint(ansiRed, "-")
	}
	return " "
}

// describe renders an action's headline, e.g. `symlink "~/.tmux.conf" -> "./files/tmux/tmux.conf"`.
func (u *UI) describe(a *Action) string {
	switch a.Type {
	case state.TypePackage:
		return fmt.Sprintf("package %q (%s)", a.Name, a.Manager)
	case state.TypeDotfile:
		return fmt.Sprintf("symlink %q -> %q", a.Target, a.Source)
	default:
		return fmt.Sprintf("service %q (%s)", a.Name, a.Scope)
	}
}

// Step announces an action being executed.
func (u *UI) Step(a *Action, verb string) {
	fmt.Fprintf(u.out, "  %s %s: %s...\n", u.symbol(a.Kind), u.describe(a), verb)
}

// StepGroup announces an operation spanning several actions.
func (u *UI) StepGroup(k Kind, msg string) {
	fmt.Fprintf(u.out, "  %s %s...\n", u.symbol(k), msg)
}

func (u *UI) RenderPlan(p *Plan) {
	fmt.Fprintln(u.out)
	for _, w := range p.Warnings {
		u.Warn("%s", w)
	}
	if len(p.Warnings) > 0 {
		fmt.Fprintln(u.out)
	}

	if !p.HasChanges() {
		fmt.Fprintln(u.out, u.paint(ansiGreen+ansiBold, "No changes.")+" Your system matches the configuration.")
		if adopt, forget := p.Count(Adopt), p.Count(Forget); adopt+forget > 0 {
			fmt.Fprintf(u.out, "State will be refreshed on apply (%d to track, %d to forget).\n", adopt, forget)
		}
		return
	}

	fmt.Fprintln(u.out, u.paint(ansiBold, "Pose will perform the following actions:"))
	for _, section := range []struct{ typ, title string }{
		{state.TypePackage, "packages"},
		{state.TypeDotfile, "dotfiles"},
		{state.TypeService, "services"},
	} {
		actions := p.filter(section.typ, Install, Link, Update, Prune)
		if len(actions) == 0 {
			continue
		}
		fmt.Fprintf(u.out, "\n  %s\n", u.paint(ansiDim, "# "+section.title))
		for _, a := range actions {
			// Package notes fit inline: `- package "htop" (pacman - will be uninstalled, ...)`.
			if a.Type == state.TypePackage && len(a.Notes) > 0 {
				fmt.Fprintf(u.out, "  %s package %q (%s - %s)\n", u.symbol(a.Kind), a.Name, a.Manager, a.Notes[0])
				continue
			}
			fmt.Fprintf(u.out, "  %s %s\n", u.symbol(a.Kind), u.describe(a))
			for _, n := range a.Notes {
				fmt.Fprintf(u.out, "      %s\n", n)
			}
		}
	}

	fmt.Fprintf(u.out, "\n%s %d to install, %d to prune, %d to link, %d to update.\n",
		u.paint(ansiBold, "Plan:"), p.Count(Install), p.Count(Prune), p.Count(Link), p.Count(Update))
}
