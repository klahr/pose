package config

// Merge layers src on top of dst:
//   - providers: merged per manager and per field, src wins
//   - packages.<manager>: concatenated and deduplicated, order preserved
//   - dotfiles: deduplicated by expanded target, src wins
//   - services: deduplicated by <scope>:<name>, src wins
//
// Both configs must already be resolved (see resolve) so targets are comparable.
func Merge(dst, src *Config) {
	if dst.Providers.PackageManagers == nil {
		dst.Providers.PackageManagers = map[string]PackageManager{}
	}
	for name, pm := range src.Providers.PackageManagers {
		cur := dst.Providers.PackageManagers[name]
		if pm.Check != "" {
			cur.Check = pm.Check
		}
		if pm.List != "" {
			cur.List = pm.List
		}
		if pm.Install != "" {
			cur.Install = pm.Install
		}
		if pm.Remove != "" {
			cur.Remove = pm.Remove
		}
		dst.Providers.PackageManagers[name] = cur
	}

	if dst.Packages == nil {
		dst.Packages = map[string][]string{}
	}
	for mgr, pkgs := range src.Packages {
		dst.Packages[mgr] = appendUnique(dst.Packages[mgr], pkgs...)
	}

	for _, d := range src.Dotfiles {
		if i := indexOf(dst.Dotfiles, func(x Dotfile) bool { return x.TargetPath == d.TargetPath }); i >= 0 {
			dst.Dotfiles[i] = d
		} else {
			dst.Dotfiles = append(dst.Dotfiles, d)
		}
	}

	for _, s := range src.Services {
		if i := indexOf(dst.Services, func(x Service) bool { return x.Key() == s.Key() }); i >= 0 {
			dst.Services[i] = s
		} else {
			dst.Services = append(dst.Services, s)
		}
	}
}

func appendUnique(list []string, items ...string) []string {
	seen := make(map[string]bool, len(list)+len(items))
	out := make([]string, 0, len(list)+len(items))
	for _, s := range append(list, items...) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func indexOf[T any](list []T, match func(T) bool) int {
	for i, x := range list {
		if match(x) {
			return i
		}
	}
	return -1
}
