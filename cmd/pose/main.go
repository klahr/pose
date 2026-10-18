// Command pose reconciles a developer machine against a declarative dotfiles repository.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/klahr/pose/internal/config"
	"github.com/klahr/pose/internal/engine"
	"github.com/klahr/pose/internal/state"
)

var version = "dev"

const usage = `pose - declarative local system reconciler

Usage:
  pose plan  [flags]        show what apply would change
  pose apply [flags]        converge the system (prompts unless --yes)
  pose state list [flags]   list resources tracked in the state file
  pose version

Flags (-C, --dir, --host, --state and --force-host may also precede the command):
  -C, --dir DIR        configuration repository (default $POSE_DIR or ~/.dotfiles)
      --host NAME      host overlay to use (default: short hostname); when given,
                       hosts/NAME.yaml must exist
      --state FILE     state file (default $XDG_DATA_HOME/pose/state.json)
      --force-host     accept a state file last written for a different host
                       (after a hostname change); resources only in the old
                       host's config will be pruned
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
		if len(args) == 0 || args[0] != "list" {
			fmt.Fprintln(os.Stderr, "usage: pose state list [flags]")
			return 2
		}
		cmd, args = "state list", args[1:]
	}

	switch cmd {
	case "help", "-h", "--help":
		fmt.Print(usage)
		return 0
	case "version", "--version":
		fmt.Println("pose", version)
		return 0
	case "plan", "apply", "state list":
	default:
		fmt.Fprintf(os.Stderr, "pose: unknown command %q\n\n%s", cmd, usage)
		return 2
	}

	fs := newFlagSet(cmd, &opts)
	if code, ok := parseFlags(fs, "pose "+cmd, args); !ok {
		return code
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "pose %s: unexpected argument %q\n", cmd, fs.Arg(0))
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
	fs.StringVar(&opts.host, "host", opts.host, "")
	fs.StringVar(&opts.statePath, "state", opts.statePath, "")
	fs.BoolVar(&opts.forceHost, "force-host", opts.forceHost, "")
	switch cmd {
	case "apply":
		fs.BoolVar(&opts.yes, "y", false, "")
		fs.BoolVar(&opts.yes, "yes", false, "")
	case "plan":
		fs.BoolVar(&opts.detailedExitCode, "detailed-exitcode", false, "")
	}
	return fs
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

// load reads config and state and prepares an engine.
func load(ui *engine.UI, opts options, lock bool) (*engine.Engine, *state.Store, error) {
	if os.Geteuid() == 0 {
		return nil, nil, errors.New("pose must not run as root; it calls sudo itself where needed")
	}

	dir := opts.dir
	if dir == "" {
		dir = os.Getenv("POSE_DIR")
	}
	if dir == "" {
		dir = "~/.dotfiles"
	}
	host := config.NormalizeHostname(opts.host)
	if host == "" {
		var err error
		if host, err = config.ShortHostname(); err != nil {
			return nil, nil, err
		}
	}

	home, _ := os.UserHomeDir()
	ui.Info("Loading config from %s (host: %s)", dir, host)
	cfg, err := config.Load(dir, host)
	if err != nil {
		return nil, nil, err
	}
	// A mistyped --host would otherwise silently drop the host's overlay, and
	// apply would prune everything it declares.
	if opts.host != "" && cfg.HostFile == "" {
		return nil, nil, fmt.Errorf("--host %s: no overlay %s",
			host, config.ContractHome(filepath.Join(cfg.Root, config.HostsDir, host+".yaml"), home))
	}
	if cfg.HostFile != "" {
		ui.Info("Merged host overlay %s", config.ContractHome(cfg.HostFile, home))
	}

	path, err := statePath(opts)
	if err != nil {
		return nil, nil, err
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
	ui.RenderPlan(p)
	if p.Empty() {
		return 0, nil
	}

	if p.HasChanges() && !opts.yes {
		ok, err := confirm(ui)
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
	ui.Printf("\nApply complete! %d installed, %d linked, %d updated, %d pruned.\n",
		p.Count(engine.Install), p.Count(engine.Link), p.Count(engine.Update), p.Count(engine.Prune))
	return 0, nil
}

func confirm(ui *engine.UI) (bool, error) {
	if !engine.IsTerminal(os.Stdin) {
		return false, errors.New("refusing to apply without confirmation: stdin is not a terminal (use --yes)")
	}
	ui.Printf("\nDo you want to perform these actions?\n  Only 'yes' will be accepted: ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	return strings.TrimSpace(line) == "yes", nil
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
