// Package config parses pose.yaml and host overlays into a single desired state.
package config

const (
	ScopeUser   = "user"
	ScopeSystem = "system"

	StateActive  = "active"
	StateStopped = "stopped"
)

// Config is the desired configuration for one host after merging and path resolution.
type Config struct {
	Providers Providers           `yaml:"providers"`
	Packages  map[string][]string `yaml:"packages"`
	Dotfiles  []Dotfile           `yaml:"dotfiles"`
	Services  []Service           `yaml:"services"`

	// Filled in by Load.
	Root     string `yaml:"-"` // absolute path of the repository
	Home     string `yaml:"-"` // absolute path of $HOME
	Hostname string `yaml:"-"` // short hostname used to pick the overlay
	HostFile string `yaml:"-"` // overlay that was merged, empty if none
}

type Providers struct {
	PackageManagers map[string]PackageManager `yaml:"package_managers"`
}

// PackageManager is a CLI contract. Templates may use {package} (one name) and,
// for install and remove, {packages} (all names at once, enabling batching).
// List, when set, prints every installed package one per line and replaces the
// per-package check, so managers with slow queries are asked only once.
//
// Check and list must exit 126 or 127 when the manager itself is missing (the
// shell does this for "command not found"); any other failure of check means
// "not installed". Guard pipelines with `command -v`, since a pipeline's
// status is that of its last command.
type PackageManager struct {
	Check   string `yaml:"check"`
	List    string `yaml:"list"`
	Install string `yaml:"install"`
	Remove  string `yaml:"remove"`
}

type Dotfile struct {
	Source   string `yaml:"source"`
	Target   string `yaml:"target"`
	OnChange string `yaml:"on_change"`

	// Absolute paths, resolved at parse time. Relative sources are resolved
	// against the repository root, so host overlays use the same paths as pose.yaml.
	SourcePath string `yaml:"-"`
	TargetPath string `yaml:"-"`
}

type Service struct {
	Name  string `yaml:"name"`
	Scope string `yaml:"scope"` // defaults to "user"
	State string `yaml:"state"` // defaults to "active"

	// EnabledOpt is the raw YAML value; when omitted, Enabled follows State.
	EnabledOpt *bool `yaml:"enabled"`
	Enabled    bool  `yaml:"-"`
}

// Key identifies a service across base and overlay files.
func (s Service) Key() string { return s.Scope + ":" + s.Name }

// BuiltinPackageManagers are available without declaring them; any field can be
// overridden from pose.yaml or a host overlay.
func BuiltinPackageManagers() map[string]PackageManager {
	return map[string]PackageManager{
		"pacman": {
			// Groups such as base-devel are not packages; they count as
			// installed once all of their members are.
			Check: `pacman -Q {package} >/dev/null 2>&1 || ` +
				`{ g=$(pacman -Sqg {package} 2>/dev/null) && [ -n "$g" ] && pacman -T $g >/dev/null 2>&1; } || ` +
				`{ command -v pacman >/dev/null 2>&1 || exit 127; exit 1; }`,
			// The same answers as check in one query: installed names, what they
			// provide (pacman -Q matches those too), and groups whose members all are.
			List: `command -v pacman >/dev/null 2>&1 || exit 127; ` +
				`q=$(LC_ALL=C pacman -Qi) && g=$(pacman -Sg) && g=$(pacman -Sg $g) || exit; ` +
				`printf '%s\n--\n%s\n' "$q" "$g" | awk '` +
				`$0 == "--" { s = 1; next } ` +
				`!s && $1 == "Name" { i[$3] = 1; print $3 } ` +
				`!s && $1 == "Provides" && $3 != "None" { for (k = 3; k <= NF; k++) { sub(/=.*/, "", $k); i[$k] = 1; print $k } } ` +
				`s && NF == 2 { g[$1] = 1; if (!($2 in i)) m[$1] = 1 } ` +
				`END { for (x in g) if (!(x in m)) print x }'`,
			Install: "sudo pacman -S --needed --noconfirm {packages}",
			Remove:  "sudo pacman -Rns --noconfirm {packages}",
		},
		"apt": {
			// dpkg -s also succeeds for removed packages whose config files remain.
			Check:   `command -v dpkg-query >/dev/null 2>&1 || exit 127; dpkg-query -W -f='${db:Status-Status}' {package} 2>/dev/null | grep -qx installed`,
			Install: "sudo apt-get install -y {packages}",
			Remove:  "sudo apt-get purge -y {packages}",
		},
		"brew": {
			// Both short and tap-qualified names, so either form matches.
			Check:   "brew list --formula {package} >/dev/null 2>&1",
			List:    "brew list --formula -1 && brew list --formula -1 --full-name",
			Install: "brew install --formula {packages}",
			Remove:  "brew uninstall --formula {packages}",
		},
		"brew-cask": {
			Check:   "brew list --cask {package} >/dev/null 2>&1",
			List:    "brew list --cask -1 && brew list --cask -1 --full-name",
			Install: "brew install --cask {packages}",
			Remove:  "brew uninstall --cask {packages}",
		},
		"cargo": {
			// Crate lines look like "name v1.2.3:" or "name v1.2.3 (source):"; both the
			// bare name and name@version match, so exact pins are found too.
			Check: `command -v cargo >/dev/null 2>&1 || exit 127; cargo install --list | awk -v p={package} ` +
				`'/^[^ ]/ { v = $2; sub(/^v/, "", v); sub(/:$/, "", v); if ($1 == p || $1 "@" v == p) f = 1 } END { exit !f }'`,
			List: `command -v cargo >/dev/null 2>&1 || exit 127; cargo install --list | awk ` +
				`'/^[^ ]/ { v = $2; sub(/^v/, "", v); sub(/:$/, "", v); print $1; print $1 "@" v }'`,
			Install: "cargo install {package}",
			Remove:  "cargo uninstall {package}",
		},
	}
}
