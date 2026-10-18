// Package state persists the set of resources pose manages on this machine.
package state

import "sort"

const Version = 1

const (
	TypePackage = "package"
	TypeDotfile = "dotfile"
	TypeService = "service"
	TypeScript  = "script"
)

type State struct {
	Version     int                 `json:"version"`
	LastApplied string              `json:"last_applied,omitempty"` // RFC 3339, UTC
	Hostname    string              `json:"hostname,omitempty"`
	Resources   map[string]Resource `json:"resources"`
}

// Resource is one managed object. Only the fields relevant to Type are set.
type Resource struct {
	Type string `json:"type"`

	// Adopted marks a package or service that already matched the config when
	// pose started tracking it. Pruning leaves such resources alone, so dropping
	// a line from the config never uninstalls or stops something pose did not set up.
	Adopted bool `json:"adopted,omitempty"`

	// package
	Manager string `json:"manager,omitempty"`
	ID      string `json:"id,omitempty"`

	// dotfile
	Target   string `json:"target,omitempty"`
	Source   string `json:"source,omitempty"`
	Hash     string `json:"hash,omitempty"` // source hash the on_change hook last ran for
	OnChange string `json:"on_change,omitempty"`
	Backup   string `json:"backup,omitempty"` // pre-existing file moved aside when linking

	// service
	Name    string `json:"name,omitempty"`
	Scope   string `json:"scope,omitempty"`
	State   string `json:"state,omitempty"`
	Enabled *bool  `json:"enabled,omitempty"`

	// script: Name, and Hash of the command that last succeeded
}

func New() *State {
	return &State{Version: Version, Resources: map[string]Resource{}}
}

func PackageKey(manager, id string) string { return TypePackage + ":" + manager + ":" + id }

// DotfileKey takes the target in display form (~/...), so keys survive a change of $HOME.
func DotfileKey(target string) string { return TypeDotfile + ":" + target }

func ServiceKey(scope, name string) string { return TypeService + ":" + scope + ":" + name }

func ScriptKey(name string) string { return TypeScript + ":" + name }

// Keys returns all resource keys in sorted order.
func (s *State) Keys() []string {
	keys := make([]string, 0, len(s.Resources))
	for k := range s.Resources {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
