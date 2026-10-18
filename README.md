# pose

Declarative, Terraform-style reconciler for packages, dotfiles and services on
Linux and macOS, driven by a single Git repository.

You describe the machine in YAML, `pose plan` shows what differs, and
`pose apply` makes it so. Things already on the machine are adopted, not
reinstalled, so you can point pose at an existing setup and grow from there.

## Install

Requires Go 1.22+.

```sh
make install          # installs to ~/.local/bin (override with PREFIX=...)
```

Make sure `~/.local/bin` is on your `PATH`.

## Getting started

**1. Create the configuration repository.** pose reads `~/.dotfiles` by
default, so starting there needs no setup:

```sh
mkdir ~/.dotfiles && git -C ~/.dotfiles init
```

To keep it elsewhere, pass `-C DIR` or set `POSE_DIR`. Set `POSE_DIR` where
every shell sees it (`~/.profile` or `~/.config/environment.d/`), not after the
interactive-only guard in `~/.bashrc`, or scripts and non-interactive shells
will silently fall back to `~/.dotfiles`.

**2. Describe something.** `pose edit` opens `pose.yaml` in `$VISUAL` or
`$EDITOR`, creating it if needed:

```yaml
packages:
  pacman: [git, neovim, ripgrep]

dotfiles:
  - source: ./files/gitconfig
    target: ~/.config/git/config
```

Move the real file into the repository (`mv ~/.config/git/config
~/.dotfiles/files/gitconfig`). If you leave it in place, pose backs it up when
linking and restores it if you later drop the entry.

**3. Preview and apply.**

```sh
pose plan             # dry run; prints nothing to change when in sync
pose apply            # shows the plan, asks for "yes", then applies (-y skips)
pose state list       # what pose now manages
```

`apply` needs a terminal for its prompt and for `sudo`; from scripts use `-y`.

**4. Commit**, then on a new machine: clone the repository to `~/.dotfiles`,
`make install` pose, and run `pose apply`.

Tip: to seed `packages` from an existing Arch system, `pacman -Qqentt` lists
packages you installed explicitly that nothing else depends on (`-Qqemtt` for
AUR packages).

## Repository layout

```
~/.dotfiles/
├── pose.yaml              # applies to every host
├── hosts/
│   └── <hostname>.yaml    # merged on top for that host only
└── files/                 # dotfile sources (any layout you like)
```

The host overlay is picked by short hostname (lowercased, domain removed).
Package lists are appended to the shared ones; a dotfile with the same target,
or a service with the same name and scope, replaces the shared entry. `pose plan` prints
`Merged host overlay …` when one is used. Use `pose edit --host` to open this
host's overlay, or `pose edit --host NAME` for another (created if missing).

Typical overlay content: GPU drivers and microcode, host-specific SSH entries
(e.g. a shared `~/.ssh/config` with `Include config.d/*`, and the overlay
linking `files/hosts/<host>/ssh/config` to `~/.ssh/config.d/<host>`).

## Configuration

### Packages

```yaml
packages:
  pacman: [ripgrep, fzf, tmux]
  brew: [jq]
```

Built-in package managers: `pacman` (groups such as `base-devel` count as
installed once all members are), `apt`, `brew`, `brew-cask`, `cargo`.

Others can be declared under `providers.package_managers` with `check`,
`install` and `remove` command templates, and optionally `list`, which prints
all installed packages one per line and replaces per-package checks. `check`
and `list` must exit 127 when the tool is missing (the shell does this for an
unknown command; guard pipelines with `command -v tool >/dev/null || exit 127;`),
otherwise every package would look uninstalled. For example, AUR packages via
yay:

```yaml
providers:
  package_managers:
    yay:
      check: "command -v yay >/dev/null 2>&1 || exit 127; pacman -Qm {package} >/dev/null 2>&1"
      list: "command -v yay >/dev/null 2>&1 || exit 127; pacman -Qqm"
      install: "yay -S --needed --noconfirm {packages}"
      remove: "yay -Rns --noconfirm {packages}"

packages:
  yay: [visual-studio-code-bin]
```

The manager itself is not installed by pose; bootstrap yay by hand first.

### Dotfiles

```yaml
dotfiles:
  - source: ./files/tmux/tmux.conf
    target: ~/.tmux.conf
    on_change: "tmux source-file ~/.tmux.conf"
  - source: ./files/alacritty        # directories work too
    target: ~/.config/alacritty
```

Targets are symlinks, so edits in the repository are live at once. A regular
file or directory at the target is moved to `<target>.backup.<timestamp>`
first; a symlink pointing elsewhere is replaced. `on_change` runs when the
source content changes (and once when the hook is added). Sources must exist:
a missing source is a configuration error, not a removal.

Links are created without `sudo`, so dotfiles are for your home directory, not
`/etc`. Changing a linked systemd unit or launchd plist does not reload or
restart the service by itself; use `on_change` for that, e.g.
`systemctl --user daemon-reload && systemctl --user restart syncthing`.

### Services

```yaml
services:
  - name: NetworkManager
    scope: system      # system (via sudo) | user; default user
    state: active      # active | stopped; default active
    enabled: true      # start at boot / login
```

pose only starts, stops, enables and disables units; it does not install them.
Declare the package that ships the unit too (packages are installed before
services are applied). For launchd, a job counts as `active` while it is
loaded, whether or not its process is running at the moment. Static systemd
units, which have no `[Install]` section, ignore `enabled`.

## State, adoption and removal

State lives in `$XDG_DATA_HOME/pose/state.json` (default
`~/.local/share/pose/state.json`). Do not edit it by hand.

**Removing something:** delete its entry and run `pose apply`; pose prunes
whatever it no longer sees in the config. For a dotfile, delete the entry
first and the source file after applying. With these exceptions:

- Packages and services that already matched the config when pose first saw
  them are *adopted*: removing them from config only forgets them, never
  uninstalls or stops them.
- A file that was backed up when a dotfile was linked is moved back when that
  dotfile is pruned.
- A package whose manager is no longer installed (its check or list exits
  127) is forgotten with a warning, since there is nothing to uninstall it with.

A failed prune is reported at the end and retried next run without blocking
the rest of the apply.

**Hostnames:** the state file records the host it was written for. Running
with a different host (a mistyped `--host`, or a renamed machine) is refused,
because the old host's resources would be pruned; pass `--force-host` if the
change is real. `--host NAME` also requires `hosts/NAME.yaml` to exist.

## Commands

```sh
pose plan             # show what would change (--detailed-exitcode: exit 2 on changes)
pose apply            # apply changes (prompts; -y to skip)
pose state list       # list managed resources
pose edit             # open pose.yaml in $VISUAL/$EDITOR (--host [NAME]: a host overlay)
pose version
```

Global flags: `-C DIR` (repository), `--host NAME`, `--state FILE`,
`--force-host`. Run `pose help` for details.

## License

GPL-3.0. See [LICENSE](LICENSE).
