package picker

import (
	"strings"
	"testing"
)

func TestKeys(t *testing.T) {
	l := &list{items: []Item{{Label: "git"}, {Label: "neovim"}, {Label: "nmap"}}}
	for _, k := range []string{" ", " ", "\x1b[A", "\x1b[A", " "} {
		l.key(k)
	}
	if got := selected(l); got != "neovim" {
		t.Errorf("after toggling = %q", got)
	}

	for _, k := range []string{"/", "n", "m", "\x7f", "\r", "a"} {
		l.key(k)
	}
	if got := selected(l); got != "neovim nmap" {
		t.Errorf("a on filter \"n\" = %q", got)
	}
	l.key("a")
	if got := selected(l); got != "" {
		t.Errorf("second a = %q", got)
	}

	l.key("\x1b")
	if l.filter != "" || len(l.visible()) != 3 {
		t.Errorf("esc did not clear filter %q", l.filter)
	}
	if l.key("\x1b") != cancelled || l.key("\r") != confirmed || l.key("\x03") != cancelled {
		t.Error("esc, enter or ctrl-c outcome wrong")
	}
}

func TestRenderScrollsToCursor(t *testing.T) {
	l := &list{title: "t", verb: "apply"}
	for _, s := range strings.Fields("a b c d e f g h i j") {
		l.items = append(l.items, Item{Label: "item-" + s})
	}
	l.key("G")
	out := l.render(8, 40) // 4 list rows
	if !strings.Contains(out, "item-j") || strings.Contains(out, "item-f") {
		t.Errorf("render did not scroll to the cursor:\n%q", out)
	}
}

func selected(l *list) string {
	var out []string
	for _, it := range l.items {
		if it.Selected {
			out = append(out, it.Label)
		}
	}
	return strings.Join(out, " ")
}
