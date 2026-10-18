# pose

Declarative, Terraform-style reconciler for packages, dotfiles and services on
Linux and macOS, driven by a single Git repository.

## Install

```sh
make install          # installs to ~/.local/bin (override with PREFIX=...)
```

## Usage

```sh
pose plan             # show what would change
pose apply            # apply changes (prompts; -y to skip)
pose state list       # list managed resources
```

Config is read from `~/.dotfiles` (or `$POSE_DIR`, or `-C DIR`):
`pose.yaml` applies to every host, and `hosts/<hostname>.yaml` is merged on top.
State lives in `$XDG_DATA_HOME/pose/state.json`. Resources removed from config
are pruned on the next apply, with two exceptions: packages and services that
already matched the config when pose first saw them are only forgotten, never
uninstalled or stopped; and a file that was backed up when a dotfile was linked
is moved back when that dotfile is pruned. A failed prune is reported at the
end and retried next run without blocking the rest of the apply.

The state file records the host it was written for. Running with a different
host (a mistyped `--host`, or a renamed machine) is refused, because the old
host's resources would be pruned; pass `--force-host` if the change is real.
`--host NAME` also requires `hosts/NAME.yaml` to exist.

```yaml
packages:
  pacman: [ripgrep, fzf, tmux]

dotfiles:
  - source: ./files/tmux/tmux.conf
    target: ~/.tmux.conf
    on_change: "tmux source-file ~/.tmux.conf"

services:
  - name: syncthing
    scope: user        # user | system
    state: active      # active | stopped
    enabled: true
```

Dotfile targets are symlinks, so edits in the repository are live at once;
`on_change` runs when the source content changes (and once when the hook is
added). Changing a linked systemd unit or launchd plist does not reload or
restart the service by itself; use `on_change` for that, e.g.
`systemctl --user daemon-reload && systemctl --user restart syncthing`.

Built-in package managers: `pacman` (groups such as `base-devel` count as
installed once all members are), `apt`, `brew`, `brew-cask`, `cargo`. Others
can be declared under `providers.package_managers` with `check`, `install` and
`remove` command templates, and optionally `list`, which prints all installed
packages one per line and replaces per-package checks. `check` and `list` must
exit 127 when the tool is missing (the shell does this for an unknown command;
guard pipelines with `command -v tool >/dev/null || exit 127;`), otherwise every
package would look uninstalled.

For launchd, a job counts as `active` while it is loaded, whether or not its
process is running at the moment. Static systemd units, which have no
`[Install]` section, ignore `enabled`.

## License

GPL-3.0. See [LICENSE](LICENSE).
