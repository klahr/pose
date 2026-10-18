# pose

Declarative, Terraform-style reconciler for packages, dotfiles and services on
Linux, driven by a single Git repository.

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

Or let pose do the editing:

```sh
pose add ~/.config/git/config     # moves it to files/.config/git/config, adds the entry, links it back
pose package add neovim ripgrep   # -m MANAGER when more than one is in use
```

Both keep the file's comments but normalize indentation to two spaces. With
`--host` (or `--host=NAME`) they write to a host overlay instead.

**3. Preview and apply.**

```sh
pose plan             # dry run; prints nothing to change when in sync
pose apply            # shows the plan, asks for "yes", then applies (-y skips)
pose state list       # what pose now manages
```

`apply` needs a terminal for its prompt and for `sudo`; from scripts use `-y`.

**4. Commit**, then on a new machine: clone the repository to `~/.dotfiles`,
`make install` pose, and run `pose apply`.

Tip: to seed `packages` from an existing system, `pose package unmanaged` lists
packages you installed on request that the config does not declare yet, one
per line on stdout:

```sh
pose package unmanaged -i          # pick from a list; the picks are added to pose.yaml
pose package unmanaged -m pacman | xargs pose package add -m pacman
```

## Repository layout

```
~/.dotfiles/
├── pose.yaml              # applies to every host
├── hosts/
│   └── <hostname>.yaml    # merged on top for that host only
├── groups/                # optional, shared by several hosts via include
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

For configuration shared by some hosts but not all, put it in its own file and
include it:

```yaml
# hosts/thinkpad.yaml
include: [groups/laptop.yaml, groups/work.yaml]
packages:
  pacman: [intel-ucode]
```

Included files are merged in order, just before the file that includes them,
so the including file wins. Paths are relative to the repository. `pose.yaml`
can include files too; included files cannot include others.

## Configuration

### Packages

```yaml
packages:
  pacman: [ripgrep, fzf, tmux]
  brew: [jq]
```

Built-in package managers: `pacman` (groups such as `base-devel` count as
installed once all members are), `apt`, `brew`, `cargo`.

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
      explicit: "command -v yay >/dev/null 2>&1 || exit 127; pacman -Qqemtt"
      install: "yay -S --needed --noconfirm {packages}"
      remove: "yay -Rns --noconfirm {packages}"

packages:
  yay: [visual-studio-code-bin]
```

`explicit` is optional: it lists packages installed on request, for
`pose package unmanaged`. The built-in managers define it.

The manager itself is not installed by pose; bootstrap it with a `before`
[script](#scripts).

### Dotfiles

```yaml
dotfiles:
  - source: ./files/tmux/tmux.conf
    target: ~/.tmux.conf
    on_change: "tmux source-file ~/.tmux.conf"
  - source: ./files/alacritty        # directories work too
    target: ~/.config/alacritty
```

With `recursive: true` the source must be a directory, and each file below it
is linked on its own, so the target stays a real directory: whatever an app
writes there (caches, state) stays out of the repository. Deleting a file from
the source unlinks it on the next apply.

Targets are symlinks, so edits in the repository are live at once. A regular
file or directory at the target is moved to `<target>.backup.<timestamp>`
first; a symlink pointing elsewhere is replaced. `on_change` runs when the
source content changes (and once when the hook is added). Sources must exist:
a missing source is a configuration error, not a removal.

Links are created without `sudo`, so dotfiles are for your home directory, not
`/etc`. Changing a linked systemd unit does not reload or restart the service
by itself; use `on_change` for that, e.g.
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
services are applied). Static systemd units, which have no `[Install]` section,
ignore `enabled`.

### Scripts

```yaml
scripts:
  - name: yay
    phase: before      # before | after; default after
    run: |
      command -v yay >/dev/null && exit 0
      git clone https://aur.archlinux.org/yay-bin.git /tmp/yay-bin
      cd /tmp/yay-bin && makepkg -si --noconfirm
  - name: shell
    run: chsh -s /usr/bin/zsh
```

A script runs through `sh` in the repository the first time pose sees it, and
again whenever its `run` text changes. `before` scripts run ahead of package
installs; `after` scripts run once services are applied. A failed script is
retried on the next apply. Removing a script from the config only forgets it.
Scripts also run on a machine that is already set up, so make them safe to run
twice (like the `command -v yay` guard above).

## State, adoption and removal

State lives in `$XDG_DATA_HOME/pose/state.json` (default
`~/.local/share/pose/state.json`). Do not edit it by hand.

**Removing something:** delete its entry and run `pose apply`; pose prunes
whatever it no longer sees in the config. For a dotfile, delete the entry
first and the source file after applying. With these exceptions:

- Packages that were already installed, and services that were already running
  or enabled, when pose first saw them are *adopted*: removing them from config
  only forgets them, never uninstalls or stops them.
- Moving a package to another package manager forgets it under the old one
  instead of uninstalling it.
- A file that was backed up when a dotfile was linked is moved back when that
  dotfile is pruned.
- A package whose manager is no longer installed (its check or list exits
  127) is forgotten with a warning, since there is nothing to uninstall it with.

A failed prune is reported at the end and retried next run without blocking
the rest of the apply.

**Keeping something but no longer managing it:** delete its entry, then
`pose state rm KEY` (keys as `pose state list` prints them) before the next
apply. The package stays installed, the symlink stays in place and the service
keeps running. `state rm` on an entry that is still in the config makes the
next apply track it again as adopted, so later dropping the entry leaves it in
place.

**Hostnames:** the state file records the host it was written for. Running
with a different host (a mistyped `--host`, or a renamed machine) is refused,
because the old host's resources would be pruned; pass `--force-host` if the
change is real. `--host NAME` also requires `hosts/NAME.yaml` to exist.

## Commands

```sh
pose plan             # show what would change (--detailed-exitcode: exit 2 on changes)
pose apply            # apply changes (prompts; -y to skip, -i to pick actions from a list)
pose state list       # list managed resources
pose state rm KEY...  # stop tracking resources without touching them
pose edit             # open pose.yaml in $VISUAL/$EDITOR (--host [NAME]: a host overlay)
pose add PATH         # move a file into the repository and manage it
pose package add PKG...   # declare packages (-m MANAGER)
pose package unmanaged    # list installed packages the config does not declare
pose completion bash|zsh|fish
pose version
```

`plan` and `apply` take `--target KEY` (repeatable) to act on only part of the
config: a type (`package`), a manager (`package:pacman`), a directory of
dotfiles (`dotfile:~/.config/nvim`) or one key from `pose state list`.

Shell completion: `pose completion bash > ~/.local/share/bash-completion/completions/pose`,
`pose completion zsh > "${fpath[1]}/_pose"`, or
`pose completion fish > ~/.config/fish/completions/pose.fish`.

Global flags: `-C DIR` (repository), `--host NAME`, `--state FILE`,
`--force-host`. Run `pose help` for details.

## License

GPL-3.0. See [LICENSE](LICENSE).
