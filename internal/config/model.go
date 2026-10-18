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
type PackageManager struct {
	Check   string `yaml:"check"`
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
			Check:   "pacman -Q {package} >/dev/null 2>&1",
			Install: "sudo pacman -S --needed --noconfirm {packages}",
			Remove:  "sudo pacman -Rns --noconfirm {packages}",
		},
		"apt": {
			Check:   "dpkg -s {package} >/dev/null 2>&1",
			Install: "sudo apt-get install -y {packages}",
			Remove:  "sudo apt-get purge -y {packages}",
		},
		"brew": {
			Check:   "brew list --formula | grep -qxF {package}",
			Install: "brew install {packages}",
			Remove:  "brew uninstall {packages}",
		},
		"cargo": {
			Check:   "cargo install --list | grep -q '^'{package}' '",
			Install: "cargo install {package}",
			Remove:  "cargo uninstall {package}",
		},
	}
}
