// Command pose reconciles a developer machine against a declarative dotfiles repository.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"

	"github.com/klahr/pose/internal/config"
	"github.com/klahr/pose/internal/engine"
	"github.com/klahr/pose/internal/picker"
	"github.com/klahr/pose/internal/providers"
	"github.com/klahr/pose/internal/state"
)

var version = "dev"

const usage = `pose - declarative local system reconciler

Usage:
  pose plan  [flags]        show what apply would change
  pose apply [flags]        converge the system (prompts unless --yes)
  pose state list [flags]   list resources tracked in the state file
  pose state rm KEY...      stop tracking resources without touching the system;
                            KEY as printed by state list
  pose edit [--host [NAME]] open pose.yaml, or with --host the host overlay
                            (default: this host's), in $VISUAL or $EDITOR
  pose add [--host[=NAME]] PATH
                            move PATH into the repository under files/, add it
                            to pose.yaml (or the host overlay) and link it back
  pose package add [-m MANAGER] [--host[=NAME]] PKG...
                            add packages to pose.yaml (or the host overlay);
                            MANAGER defaults to the only one in use
  pose package unmanaged [-m MANAGER]
                            list packages installed on request but not declared
  pose completion bash|zsh|fish
                            print a shell completion script
  pose version

Flags (-C, --dir, --host, --state and --force-host may also precede the command):
  -C, --dir DIR        configuration repository (default $POSE_DIR or ~/.dotfiles)
      --host NAME      host overlay to use (default: short hostname); when given,
                       hosts/NAME.yaml must exist
      --state FILE     state file (default $XDG_DATA_HOME/pose/state.json)
      --force-host     accept a state file last written for a different host
                       (after a hostname change); resources only in the old
                       host's config will be pruned
      --target KEY     plan, apply: only act on KEY or what lies below it, e.g.
                       package, package:pacman, dotfile:~/.config/nvim,
                       service:user:syncthing; repeatable
  -i, --interactive    apply: pick the actions to apply from a list (instead of
                       the confirmation prompt); package unmanaged: pick the
                       packages to add to pose.yaml
  -y, --yes            apply: skip the confirmation prompt
      --detailed-exitcode
                       plan: exit 2 when there are changes, 0 when there are none

Environment:
  NO_COLOR             disable colored output when set to a non-empty value
`

type options struct {
	dir, host, statePath string
	forceHost            bool
	yes                  bool
	detailedExitCode     bool
	editHost             bool
	targets              []string
	manager              string
	interactive          bool
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	var opts options
	if len(args) > 0 && strings.HasPrefix(args[0], "-") && !isMeta(args[0]) {
		fs := newFlagSet("", &opts)
		if code, ok := parseFlags(fs, "pose", args); !ok {
			return code
		}
		args = fs.Args()
	}
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	cmd, args := args[0], args[1:]
	if cmd == "state" {
		if len(args) == 0 || args[0] != "list" && args[0] != "rm" {
			fmt.Fprintln(os.Stderr, "usage: pose state list|rm [flags]")
			return 2
		}
		cmd, args = "state "+args[0], args[1:]
	}
	if cmd == "package" {
		if len(args) == 0 || args[0] != "add" && args[0] != "unmanaged" {
			fmt.Fprintln(os.Stderr, "usage: pose package add|unmanaged [flags]")
			return 2
		}
		cmd, args = "package "+args[0], args[1:]
	}

	switch cmd {
	case "help", "-h", "--help":
		fmt.Print(usage)
		return 0
	case "version", "--version":
		fmt.Println("pose", version)
		return 0
	case "completion":
		return cmdCompletion(args)
	case "plan", "apply", "state list", "state rm", "edit", "add", "package add", "package unmanaged":
	default:
		fmt.Fprintf(os.Stderr, "pose: unknown command %q\n\n%s", cmd, usage)
		return 2
	}

	fs := newFlagSet(cmd, &opts)
	if code, ok := parseFlags(fs, "pose "+cmd, args); !ok {
		return code
	}
	rest := fs.Args()
	// "edit --host NAME": the bare --host parses as a boolean, so NAME is left over.
	if cmd == "edit" && opts.editHost && opts.host == "" && len(rest) == 1 {
		opts.host, rest = rest[0], nil
	}
	switch {
	case cmd == "state rm" && len(rest) == 0:
		fmt.Fprintln(os.Stderr, "usage: pose state rm KEY...")
		return 2
	case cmd == "add" && len(rest) != 1:
		fmt.Fprintln(os.Stderr, "usage: pose add [--host[=NAME]] PATH")
		return 2
	case cmd == "package add" && len(rest) == 0:
		fmt.Fprintln(os.Stderr, "usage: pose package add [-m MANAGER] [--host[=NAME]] PKG...")
		return 2
	case cmd != "state rm" && cmd != "add" && cmd != "package add" && len(rest) > 0:
		fmt.Fprintf(os.Stderr, "pose %s: unexpected argument %q\n", cmd, rest[0])
		return 2
	}

	ui := engine.NewUI(os.Stdout, os.Stderr)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// After the first interrupt, restore default handling so a second one exits immediately.
	go func() {
		<-ctx.Done()
		stop()
	}()

	var err error
	code := 0
	switch cmd {
	case "plan":
		code, err = cmdPlan(ctx, ui, opts)
	case "apply":
		code, err = cmdApply(ctx, ui, opts)
	case "state list":
		err = cmdStateList(ui, opts)
	case "state rm":
		err = cmdStateRm(ui, opts, rest)
	case "edit":
		err = cmdEdit(ui, opts)
	case "add":
		err = cmdAdd(ui, opts, rest[0])
	case "package add":
		err = cmdPackageAdd(ui, opts, rest)
	case "package unmanaged":
		// stdout carries only package names, so the list can be piped.
		err = cmdPackageUnmanaged(ctx, engine.NewUI(os.Stderr, os.Stderr), opts)
	}
	if err != nil {
		ui.Error("%v", err)
		return 1
	}
	return code
}

func isMeta(arg string) bool {
	switch arg {
	case "-h", "-help", "--help", "-version", "--version":
		return true
	}
	return false
}

// newFlagSet registers the global flags plus those of cmd ("" for flags given
// before the command). Current values serve as defaults, so flags given before
// the command survive the second parse.
func newFlagSet(cmd string, opts *options) *flag.FlagSet {
	fs := flag.NewFlagSet("pose "+cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&opts.dir, "C", opts.dir, "")
	fs.StringVar(&opts.dir, "dir", opts.dir, "")
	if cmd == "edit" || cmd == "add" || cmd == "package add" {
		fs.Var(hostFlag{opts}, "host", "")
	} else {
		fs.StringVar(&opts.host, "host", opts.host, "")
	}
	fs.StringVar(&opts.statePath, "state", opts.statePath, "")
	fs.BoolVar(&opts.forceHost, "force-host", opts.forceHost, "")
	switch cmd {
	case "apply":
		fs.BoolVar(&opts.yes, "y", false, "")
		fs.BoolVar(&opts.yes, "yes", false, "")
		fs.BoolVar(&opts.interactive, "i", false, "")
		fs.BoolVar(&opts.interactive, "interactive", false, "")
		fs.Var(targetFlag{opts}, "target", "")
	case "plan":
		fs.BoolVar(&opts.detailedExitCode, "detailed-exitcode", false, "")
		fs.Var(targetFlag{opts}, "target", "")
	case "package add", "package unmanaged":
		fs.StringVar(&opts.manager, "m", "", "")
		fs.StringVar(&opts.manager, "manager", "", "")
		if cmd == "package unmanaged" {
			fs.BoolVar(&opts.interactive, "i", false, "")
			fs.BoolVar(&opts.interactive, "interactive", false, "")
		}
	}
	return fs
}

type targetFlag struct{ opts *options }

func (f targetFlag) String() string { return "" }
func (f targetFlag) Set(v string) error {
	if v == "" {
		return errors.New("empty target")
	}
	f.opts.targets = append(f.opts.targets, v)
	return nil
}

// hostFlag is edit's --host: alone it selects this host's overlay, with a
// value (--host=NAME, or NAME as the next argument) it names the host.
type hostFlag struct{ opts *options }

func (f hostFlag) String() string   { return "" }
func (f hostFlag) IsBoolFlag() bool { return true }
func (f hostFlag) Set(v string) error {
	switch v {
	case "true":
		f.opts.editHost = true
	case "false":
		f.opts.editHost, f.opts.host = false, ""
	default:
		f.opts.editHost, f.opts.host = true, v
	}
	return nil
}

// parseFlags parses args into fs. When it returns false, the caller exits with code.
func parseFlags(fs *flag.FlagSet, prefix string, args []string) (code int, ok bool) {
	err := fs.Parse(args)
	switch {
	case err == nil:
		return 0, true
	case errors.Is(err, flag.ErrHelp):
		fmt.Print(usage)
		return 0, false
	}
	fmt.Fprintf(os.Stderr, "%s: %v\n", prefix, err)
	return 2, false
}

func statePath(opts options) (string, error) {
	if opts.statePath != "" {
		return filepath.Abs(opts.statePath)
	}
	return state.DefaultPath()
}

func configDir(opts options) string {
	if opts.dir != "" {
		return opts.dir
	}
	if dir := os.Getenv("POSE_DIR"); dir != "" {
		return dir
	}
	return "~/.dotfiles"
}

func hostName(opts options) (string, error) {
	if opts.host == "" {
		return config.ShortHostname()
	}
	// A name that normalizes to nothing ("../x" is cut at its first dot) must not
	// fall back to this host's overlay.
	if host := config.NormalizeHostname(opts.host); host != "" {
		return host, nil
	}
	return "", fmt.Errorf("invalid hostname %q", opts.host)
}

// loadConfig reads and merges the configuration for the selected host.
func loadConfig(ui *engine.UI, opts options) (*config.Config, error) {
	if os.Geteuid() == 0 {
		return nil, errors.New("pose must not run as root; it calls sudo itself where needed")
	}

	dir := configDir(opts)
	host, err := hostName(opts)
	if err != nil {
		return nil, err
	}

	ui.Info("Loading config from %s (host: %s)", dir, host)
	cfg, err := config.Load(dir, host)
	if err != nil {
		return nil, err
	}
	home := cfg.Home
	// A mistyped --host would otherwise silently drop the host's overlay, and
	// apply would prune everything it declares.
	if opts.host != "" && cfg.HostFile == "" {
		return nil, fmt.Errorf("--host %s: no overlay %s",
			host, config.ContractHome(filepath.Join(cfg.Root, config.HostsDir, host+".yaml"), home))
	}
	for _, p := range cfg.Included {
		ui.Info("Merged include %s", config.ContractHome(p, home))
	}
	if cfg.HostFile != "" {
		ui.Info("Merged host overlay %s", config.ContractHome(cfg.HostFile, home))
	}
	return cfg, nil
}

// load reads config and state and prepares an engine.
func load(ui *engine.UI, opts options, lock bool) (*engine.Engine, *state.Store, error) {
	cfg, err := loadConfig(ui, opts)
	if err != nil {
		return nil, nil, err
	}
	home, host := cfg.Home, cfg.Hostname

	path, err := statePath(opts)
	if err != nil {
		return nil, nil, err
	}
	// Linking over either would move it into a backup and redirect it into the repository.
	protected := []string{path}
	if exe, err := os.Executable(); err == nil {
		protected = append(protected, exe)
	}
	for _, p := range protected {
		if d, ok := cfg.TargetContaining(p); ok {
			return nil, nil, fmt.Errorf("dotfile %s: target must not contain %s", d.Target, config.ContractHome(p, home))
		}
	}
	store, err := state.Open(path, lock)
	if err != nil {
		return nil, nil, err
	}
	if prev := store.State.Hostname; prev != "" && prev != host {
		if !opts.forceHost {
			store.Close()
			return nil, nil, fmt.Errorf("state file %s was last written for host %q, not %q.\n"+
				"Resources only in that host's config would be pruned. Use --host %s, a separate --state\n"+
				"file, or --force-host if this machine's hostname really changed", path, prev, host, prev)
		}
		ui.Warn("state file %s was last written for host %q, now running as %q", path, prev, host)
	}

	eng, err := engine.New(cfg, store, ui)
	if err != nil {
		store.Close()
		return nil, nil, err
	}
	return eng, store, nil
}

func cmdPlan(ctx context.Context, ui *engine.UI, opts options) (int, error) {
	eng, store, err := load(ui, opts, false)
	if err != nil {
		return 0, err
	}
	defer store.Close()

	ui.Info("Inspecting system state...")
	p, err := eng.Plan(ctx)
	if err != nil {
		return 0, err
	}
	if len(opts.targets) > 0 {
		if err := eng.Target(p, opts.targets); err != nil {
			return 0, err
		}
	}
	ui.RenderPlan(p)
	if opts.detailedExitCode && p.HasChanges() {
		return 2, nil
	}
	return 0, nil
}

func cmdApply(ctx context.Context, ui *engine.UI, opts options) (int, error) {
	eng, store, err := load(ui, opts, true)
	if err != nil {
		return 0, err
	}
	defer store.Close()

	ui.Info("Inspecting system state...")
	p, err := eng.Plan(ctx)
	if err != nil {
		return 0, err
	}
	if len(opts.targets) > 0 {
		if err := eng.Target(p, opts.targets); err != nil {
			return 0, err
		}
	}
	if opts.interactive && p.HasChanges() {
		ok, err := pickActions(ui, p)
		if err != nil {
			return 0, err
		}
		if !ok {
			ui.Printf("Apply cancelled.\n")
			return 1, nil
		}
	}
	ui.RenderPlan(p)
	if p.Empty() {
		// Record the new hostname, or every later run fails the host check again.
		if opts.forceHost {
			return 0, store.Save()
		}
		return 0, nil
	}

	if p.HasChanges() && !opts.yes && !opts.interactive {
		ok, err := confirm(ctx, ui)
		if err != nil {
			return 0, err
		}
		if !ok {
			ui.Printf("\nApply cancelled.\n")
			return 1, nil
		}
	}

	ui.Printf("\n")
	if err := eng.Apply(ctx, p); err != nil {
		return 0, fmt.Errorf("apply failed: %w\nState has been saved for completed actions; run `pose plan` to see what remains", err)
	}
	ui.Printf("\nApply complete! %d installed, %d linked, %d updated, %d pruned, %d run.\n",
		p.Count(engine.Install), p.Count(engine.Link), p.Count(engine.Update), p.Count(engine.Prune), p.Count(engine.Run))
	return 0, nil
}

// pickActions lets the user deselect changes; bookkeeping actions always stay.
func pickActions(ui *engine.UI, p *engine.Plan) (bool, error) {
	var changes []*engine.Action
	var items []picker.Item
	for _, a := range p.Actions {
		if a.Kind.Changes() {
			changes = append(changes, a)
			items = append(items, picker.Item{Label: ui.Headline(a), Selected: true})
		}
	}
	ok, err := picker.Run(os.Stdin, os.Stderr, "Select the actions to apply", "apply", items)
	if err != nil || !ok {
		return false, err
	}
	drop := map[*engine.Action]bool{}
	for i, it := range items {
		drop[changes[i]] = !it.Selected
	}
	p.Actions = slices.DeleteFunc(p.Actions, func(a *engine.Action) bool { return drop[a] })
	return true, nil
}

func confirm(ctx context.Context, ui *engine.UI) (bool, error) {
	if !engine.IsTerminal(os.Stdin) {
		return false, errors.New("refusing to apply without confirmation: stdin is not a terminal (use --yes)")
	}
	ui.Printf("\nDo you want to perform these actions?\n  Only 'yes' will be accepted: ")
	type answer struct {
		line string
		err  error
	}
	ch := make(chan answer, 1)
	go func() {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		ch <- answer{line, err}
	}()
	select {
	case <-ctx.Done(): // Ctrl-C at the prompt
		return false, nil
	case a := <-ch:
		if a.err != nil && !errors.Is(a.err, io.EOF) {
			return false, a.err
		}
		return strings.TrimSpace(a.line) == "yes", nil
	}
}

func cmdStateList(ui *engine.UI, opts options) error {
	path, err := statePath(opts)
	if err != nil {
		return err
	}
	store, err := state.Open(path, false)
	if err != nil {
		return err
	}
	keys := store.State.Keys()
	if len(keys) == 0 {
		fmt.Fprintf(os.Stderr, "No resources tracked in %s\n", path)
		return nil
	}
	for _, k := range keys {
		ui.Printf("%s\n", k)
	}
	return nil
}

// cmdStateRm forgets resources. One still in the config is tracked again by the
// next apply, as adopted if the system still has it, so pruning it later
// leaves it in place.
func cmdStateRm(ui *engine.UI, opts options, keys []string) error {
	path, err := statePath(opts)
	if err != nil {
		return err
	}
	store, err := state.Open(path, true)
	if err != nil {
		return err
	}
	defer store.Close()
	for _, k := range keys {
		if _, ok := store.State.Resources[k]; !ok {
			return fmt.Errorf("%s is not tracked in %s; see pose state list", k, path)
		}
	}
	if err := store.Update(func(st *state.State) {
		for _, k := range keys {
			delete(st.Resources, k)
		}
	}); err != nil {
		return err
	}
	for _, k := range keys {
		ui.Info("Forgot %s", k)
	}
	return nil
}

// configFile resolves the file edit, add and package add change: pose.yaml, or
// with --host a host overlay. Either may be new, but the repository must exist.
// host is the one whose merged config the result must validate as.
func configFile(opts options) (path, root, home, host string, err error) {
	if os.Geteuid() == 0 {
		return "", "", "", "", errors.New("pose must not run as root; it calls sudo itself where needed")
	}
	if home, err = os.UserHomeDir(); err != nil {
		return "", "", "", "", fmt.Errorf("determine home directory: %w", err)
	}
	home = filepath.Clean(home)
	if root, err = config.ExpandHome(configDir(opts), home); err != nil {
		return "", "", "", "", err
	}
	if root, err = filepath.Abs(root); err != nil {
		return "", "", "", "", err
	}
	if fi, err := os.Stat(root); errors.Is(err, fs.ErrNotExist) || err == nil && !fi.IsDir() {
		return "", "", "", "", fmt.Errorf("configuration repository %s does not exist", config.ContractHome(root, home))
	} else if err != nil {
		return "", "", "", "", err
	}
	if host, err = hostName(opts); err != nil {
		return "", "", "", "", err
	}

	path = filepath.Join(root, config.BaseFile)
	if opts.editHost || opts.host != "" {
		if !config.ValidHostname(host) {
			return "", "", "", "", fmt.Errorf("invalid hostname %q", host)
		}
		path = filepath.Join(root, config.HostsDir, host+".yaml")
	}
	return path, root, home, host, nil
}

// cmdEdit opens pose.yaml, or a host overlay, in the user's editor.
func cmdEdit(ui *engine.UI, opts options) error {
	path, _, home, _, err := configFile(opts)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "vi"
	}
	ui.Info("Editing %s", config.ContractHome(path, home))
	// Through sh, so editors given with arguments ("code --wait") work.
	cmd := exec.Command("sh", "-c", editor+` "$1"`, "sh", path)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("editor %q: %w", editor, err)
	}
	return nil
}

// cmdAdd moves an existing file or directory into the repository, declares it
// and links it back, so the next apply only starts tracking it.
func cmdAdd(ui *engine.UI, opts options, arg string) error {
	path, root, home, host, err := configFile(opts)
	if err != nil {
		return err
	}
	target, err := config.ExpandHome(arg, home)
	if err != nil {
		return err
	}
	if target, err = filepath.Abs(target); err != nil {
		return err
	}
	display := config.ContractHome(target, home)
	fi, err := os.Lstat(target)
	if err != nil {
		return err
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink; add the file it points to", display)
	}
	rel, err := filepath.Rel(home, target)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return fmt.Errorf("%s: only paths inside %s can be added", display, home)
	}
	// Through a linked parent the path may already lie in the repository.
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	realParent, err := filepath.EvalSymlinks(filepath.Dir(target))
	if err != nil {
		return err
	}
	for _, p := range []string{filepath.Join(realParent, fi.Name()), target} {
		if r, err := filepath.Rel(realRoot, p); err == nil && r != ".." && !strings.HasPrefix(r, "../") {
			return fmt.Errorf("%s is inside the repository", display)
		}
		if r, err := filepath.Rel(p, realRoot); err == nil && r != ".." && !strings.HasPrefix(r, "../") {
			return fmt.Errorf("%s contains the repository", display)
		}
	}

	source := filepath.Join(root, "files", rel)
	if _, err := os.Lstat(source); err == nil {
		return fmt.Errorf("%s already exists", config.ContractHome(source, home))
	}
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		return err
	}
	if err := os.Rename(target, source); err != nil {
		return fmt.Errorf("move %s into the repository: %w", display, err)
	}
	moveBack := func() {
		if err := os.Rename(source, target); err != nil {
			ui.Error("could not move %s back to %s: %v", source, display, err)
		}
	}
	undo, err := config.AddDotfile(path, "./"+filepath.ToSlash(filepath.Join("files", rel)), display)
	if err == nil {
		if _, err = config.Load(root, host); err != nil {
			if uerr := undo(); uerr != nil {
				ui.Error("could not restore %s: %v", path, uerr)
			}
		}
	}
	if err != nil {
		moveBack()
		return err
	}
	if err := os.Symlink(source, target); err != nil {
		return fmt.Errorf("link %s (declared in %s; pose apply links it): %w", display, config.ContractHome(path, home), err)
	}
	ui.Info("Moved %s to %s, linked it and added it to %s",
		display, config.ContractHome(source, home), config.ContractHome(path, home))
	return nil
}

// cmdPackageAdd declares packages; the next apply installs them.
func cmdPackageAdd(ui *engine.UI, opts options, pkgs []string) error {
	path, root, home, host, err := configFile(opts)
	if err != nil {
		return err
	}
	mgr := opts.manager
	if mgr == "" {
		cfg, err := config.Load(root, host)
		if err != nil {
			return fmt.Errorf("%w\n(pass -m MANAGER to add packages without reading the config)", err)
		}
		var used []string
		for _, m := range sortedKeys(cfg.Packages) {
			if len(cfg.Packages[m]) > 0 {
				used = append(used, m)
			}
		}
		if len(used) != 1 {
			return fmt.Errorf("cannot pick a package manager (in use: %s); pass -m MANAGER", strings.Join(used, ", "))
		}
		mgr = used[0]
	}

	return declarePackages(ui, path, root, home, host, mgr, pkgs)
}

// declarePackages adds pkgs to packages.<mgr> in path, undoing the edit if the
// config for host no longer loads.
func declarePackages(ui *engine.UI, path, root, home, host, mgr string, pkgs []string) error {
	added, undo, err := config.AddPackages(path, mgr, pkgs)
	if err != nil {
		return err
	}
	if _, err := config.Load(root, host); err != nil {
		if uerr := undo(); uerr != nil {
			ui.Error("could not restore %s: %v", path, uerr)
		}
		return err
	}
	file := config.ContractHome(path, home)
	if len(added) == 0 {
		ui.Info("All already listed under packages.%s in %s", mgr, file)
		return nil
	}
	ui.Info("Added %s to packages.%s in %s; run pose apply to install", strings.Join(added, ", "), mgr, file)
	return nil
}

// cmdPackageUnmanaged prints packages the user installed that no manager in the
// config declares, one per line on stdout; everything else goes to stderr.
func cmdPackageUnmanaged(ctx context.Context, ui *engine.UI, opts options) error {
	cfg, err := loadConfig(ui, opts)
	if err != nil {
		return err
	}
	managers := sortedKeys(cfg.Providers.PackageManagers)
	if opts.manager != "" {
		if _, ok := cfg.Providers.PackageManagers[opts.manager]; !ok {
			return fmt.Errorf("no package manager %q defined", opts.manager)
		}
		managers = []string{opts.manager}
	}
	// Any manager counts: an AUR helper may declare what pacman lists.
	declared := map[string]bool{}
	for _, pkgs := range cfg.Packages {
		for _, p := range pkgs {
			name, _, _ := strings.Cut(p, "@") // cargo version pins
			declared[p], declared[name] = true, true
		}
	}
	type candidate struct{ mgr, pkg string }
	var found []candidate
	for _, name := range managers {
		pkgs, ok, err := providers.NewPackageManager(name, cfg.Providers.PackageManagers[name]).Explicit(ctx)
		switch {
		case !ok && opts.manager != "":
			return fmt.Errorf("package manager %s defines no explicit command", name)
		case !ok, err != nil && opts.manager == "" && providers.ManagerMissing(err):
			continue
		case err != nil:
			return fmt.Errorf("list %s packages: %w", name, err)
		}
		pkgs = slices.DeleteFunc(pkgs, func(p string) bool { return declared[p] })
		ui.Info("%s: %d not in config", name, len(pkgs))
		for _, p := range pkgs {
			if opts.interactive {
				found = append(found, candidate{name, p})
			} else {
				fmt.Println(p)
			}
		}
	}
	if !opts.interactive || len(found) == 0 {
		return nil
	}

	items := make([]picker.Item, len(found))
	for i, c := range found {
		items[i].Label = c.pkg
		if len(managers) > 1 {
			items[i].Label += "  (" + c.mgr + ")"
		}
	}
	ok, err := picker.Run(os.Stdin, os.Stderr, "Select packages to add to "+config.BaseFile, "add", items)
	if err != nil || !ok {
		return err
	}
	byManager := map[string][]string{}
	for i, it := range items {
		if it.Selected {
			byManager[found[i].mgr] = append(byManager[found[i].mgr], found[i].pkg)
		}
	}
	if len(byManager) == 0 {
		ui.Info("Nothing selected")
		return nil
	}
	path := filepath.Join(cfg.Root, config.BaseFile)
	for _, mgr := range sortedKeys(byManager) {
		if err := declarePackages(ui, path, cfg.Root, cfg.Home, cfg.Hostname, mgr, byManager[mgr]); err != nil {
			return err
		}
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
