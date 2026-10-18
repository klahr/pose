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
are pruned on the next apply.

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

Built-in package managers: `pacman`, `apt`, `brew`, `cargo`. Others can be
declared under `providers.package_managers` with `check`, `install` and
`remove` command templates.

## License

GPL-3.0. See [LICENSE](LICENSE).
