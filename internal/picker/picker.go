// Package picker is a minimal full-screen multi-select list for the terminal.
package picker

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"unicode/utf8"
	"unsafe"
)

type Item struct {
	Label    string
	Selected bool
}

// Run lets the user toggle items on the terminal behind in and out (drawn on
// the alternate screen) and reports whether they confirmed; on return each
// item's Selected holds their choice. verb names what enter does ("apply").
func Run(in, out *os.File, title, verb string, items []Item) (bool, error) {
	old, err := getTermios(in)
	if err == nil {
		_, err = getTermios(out)
	}
	if err != nil {
		return false, errors.New("interactive mode needs a terminal")
	}
	raw := *old
	// ISIG off: Ctrl-C arrives as a key, so the terminal is always restored.
	raw.Lflag &^= syscall.ICANON | syscall.ECHO | syscall.ISIG | syscall.IEXTEN
	raw.Iflag &^= syscall.IXON | syscall.ICRNL
	raw.Cc[syscall.VMIN], raw.Cc[syscall.VTIME] = 1, 0
	if err := setTermios(in, &raw); err != nil {
		return false, err
	}
	fmt.Fprint(out, "\033[?1049h\033[?25l")
	defer func() {
		fmt.Fprint(out, "\033[?25h\033[?1049l")
		_ = setTermios(in, old)
	}()

	l := &list{items: items, title: title, verb: verb}
	buf := make([]byte, 64)
	for {
		rows, cols := size(out)
		fmt.Fprint(out, l.render(rows, cols))
		n, err := in.Read(buf)
		if err != nil {
			return false, err
		}
		switch l.key(string(buf[:n])) {
		case confirmed:
			return true, nil
		case cancelled:
			return false, nil
		}
	}
}

type outcome int

const (
	pending outcome = iota
	confirmed
	cancelled
)

type list struct {
	items       []Item
	title, verb string
	filter      string
	filtering   bool
	cursor      int // index into visible()
	top         int // first visible() index on screen
}

// visible returns the indexes of the items matching the filter.
func (l *list) visible() []int {
	var out []int
	f := strings.ToLower(l.filter)
	for i, it := range l.items {
		if strings.Contains(strings.ToLower(it.Label), f) {
			out = append(out, i)
		}
	}
	return out
}

func (l *list) key(k string) outcome {
	if k == "\x03" {
		return cancelled
	}
	if l.filtering {
		switch k {
		case "\r", "\n", "\x1b":
			l.filtering = false
		case "\x7f", "\x08":
			if _, n := utf8.DecodeLastRuneInString(l.filter); n > 0 {
				l.filter = l.filter[:len(l.filter)-n]
			}
		default:
			for _, r := range k {
				if r >= ' ' && r != 0x7f {
					l.filter += string(r)
				}
			}
		}
		l.cursor = 0
		return pending
	}

	vis := l.visible()
	switch k {
	case "\x1b[A", "k":
		l.cursor--
	case "\x1b[B", "j":
		l.cursor++
	case "\x1b[5~":
		l.cursor -= 10
	case "\x1b[6~":
		l.cursor += 10
	case "g", "\x1b[H":
		l.cursor = 0
	case "G", "\x1b[F":
		l.cursor = len(vis) - 1
	case " ":
		if l.cursor < len(vis) {
			it := &l.items[vis[l.cursor]]
			it.Selected = !it.Selected
			l.cursor++
		}
	case "a":
		all := true
		for _, i := range vis {
			all = all && l.items[i].Selected
		}
		for _, i := range vis {
			l.items[i].Selected = !all
		}
	case "/":
		l.filtering = true
	case "\r", "\n":
		return confirmed
	case "\x1b":
		if l.filter == "" {
			return cancelled
		}
		l.filter, l.cursor = "", 0
	case "q":
		return cancelled
	}
	l.cursor = max(0, min(l.cursor, len(vis)-1))
	return pending
}

func (l *list) render(rows, cols int) string {
	vis := l.visible()
	height := max(1, rows-4) // title, blank line, filter, help
	l.cursor = max(0, min(l.cursor, len(vis)-1))
	if l.cursor < l.top {
		l.top = l.cursor
	}
	if l.cursor >= l.top+height {
		l.top = l.cursor - height + 1
	}
	l.top = max(0, min(l.top, len(vis)-height))

	n := 0
	for _, it := range l.items {
		if it.Selected {
			n++
		}
	}
	var b strings.Builder
	b.WriteString("\033[H\033[2J")
	fmt.Fprintf(&b, "\033[1m%s\033[0m  %d of %d selected\r\n\r\n", clip(l.title, cols-24), n, len(l.items))
	for row := l.top; row < min(l.top+height, len(vis)); row++ {
		it := l.items[vis[row]]
		mark := "[ ]"
		if it.Selected {
			mark = "[x]"
		}
		line := clip(" "+mark+" "+it.Label, cols-1)
		if row == l.cursor {
			line = "\033[7m" + line + "\033[0m"
		}
		b.WriteString(line)
		b.WriteString("\r\n")
	}
	if len(vis) == 0 {
		b.WriteString(" nothing matches\r\n")
	}
	fmt.Fprintf(&b, "\033[%d;1H", rows-1)
	switch {
	case l.filtering:
		fmt.Fprintf(&b, "/%s\033[7m \033[0m", l.filter)
	case l.filter != "":
		fmt.Fprintf(&b, "\033[2mfilter: %s (esc clears)\033[0m", l.filter)
	}
	help := fmt.Sprintf("space toggle  a all  / filter  enter %s  q cancel", l.verb)
	fmt.Fprintf(&b, "\033[%d;1H\033[2m%s\033[0m", rows, clip(help, cols-1))
	return b.String()
}

// clip shortens s to at most n runes.
func clip(s string, n int) string {
	if n <= 0 || utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:max(0, n-1)]) + "…"
}

func size(f *os.File) (rows, cols int) {
	var ws struct{ Row, Col, X, Y uint16 }
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&ws)))
	if errno != 0 || ws.Row == 0 || ws.Col == 0 {
		return 24, 80
	}
	return int(ws.Row), int(ws.Col)
}

func getTermios(f *os.File) (*syscall.Termios, error) {
	var t syscall.Termios
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(syscall.TCGETS), uintptr(unsafe.Pointer(&t))); errno != 0 {
		return nil, errno
	}
	return &t, nil
}

func setTermios(f *os.File, t *syscall.Termios) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(syscall.TCSETS), uintptr(unsafe.Pointer(t))); errno != 0 {
		return errno
	}
	return nil
}
