package main

import (
	"fmt"
	"os"
)

// Completion scripts complete state keys (for state rm and --target) from
// `pose state list`.

const bashCompletion = `# bash completion for pose; needs bash-completion
_pose() {
	local cur prev words cword
	_init_completion -n : || return
	local i cmd= sub=
	for ((i = 1; i < cword; i++)); do
		case ${words[i]} in
		-C | --dir | --state | --target | -m | --manager) ((i++)) ;;
		-*) ;;
		*) if [[ -z $cmd ]]; then cmd=${words[i]}; elif [[ -z $sub ]]; then sub=${words[i]}; fi ;;
		esac
	done
	case $prev in
	-C | --dir) _filedir -d; return ;;
	--state) _filedir; return ;;
	--target)
		COMPREPLY=($(compgen -W "package dotfile service script $(pose state list 2>/dev/null)" -- "$cur"))
		__ltrim_colon_completions "$cur"
		return
		;;
	-m | --manager) return ;;
	esac
	local flags="-C --dir --host --state --force-host"
	case $cmd in
	'') COMPREPLY=($(compgen -W "plan apply state edit add package completion version help $flags" -- "$cur")) ;;
	plan) COMPREPLY=($(compgen -W "--detailed-exitcode --target $flags" -- "$cur")) ;;
	apply) COMPREPLY=($(compgen -W "-y --yes -i --interactive --target $flags" -- "$cur")) ;;
	edit) COMPREPLY=($(compgen -W "$flags" -- "$cur")) ;;
	add) if [[ $cur == -* ]]; then COMPREPLY=($(compgen -W "$flags" -- "$cur")); else _filedir; fi ;;
	state)
		case $sub in
		'') COMPREPLY=($(compgen -W "list rm" -- "$cur")) ;;
		rm)
			COMPREPLY=($(compgen -W "$(pose state list 2>/dev/null)" -- "$cur"))
			__ltrim_colon_completions "$cur"
			;;
		esac
		;;
	package)
		case $sub in
		'') COMPREPLY=($(compgen -W "add unmanaged" -- "$cur")) ;;
		add) COMPREPLY=($(compgen -W "-m --manager $flags" -- "$cur")) ;;
		*) COMPREPLY=($(compgen -W "-m --manager -i --interactive $flags" -- "$cur")) ;;
		esac
		;;
	completion) COMPREPLY=($(compgen -W "bash zsh fish" -- "$cur")) ;;
	esac
}
complete -F _pose pose
`

const zshCompletion = `#compdef pose
_pose() {
	local -a global=(
		'-C[configuration repository]:directory:_files -/'
		'--dir[configuration repository]:directory:_files -/'
		'--host[host overlay]:host:'
		'--state[state file]:file:_files'
		'--force-host[accept a state file written for another host]'
	)
	local -a keys=(${(f)"$(pose state list 2>/dev/null)"})
	local cmd i
	for ((i = 2; i < CURRENT; i++)); do
		case $words[i] in
		-C | --dir | --state | --target | -m | --manager) ((i++)) ;;
		-*) ;;
		*) cmd=$words[i]; break ;;
		esac
	done
	case $cmd in
	'') _arguments $global '*::command:(plan apply state edit add package completion version help)' ;;
	plan) _arguments $global '--detailed-exitcode[exit 2 when there are changes]' '*--target[only act on KEY]:key:(package dotfile service script $keys)' ;;
	apply) _arguments $global '(-y --yes)'{-y,--yes}'[skip the confirmation prompt]' '(-i --interactive)'{-i,--interactive}'[pick actions from a list]' '*--target[only act on KEY]:key:(package dotfile service script $keys)' ;;
	edit) _arguments $global ;;
	add) _arguments $global '1:path:_files' ;;
	state) _arguments $global '1:subcommand:(list rm)' '*:key:(${keys})' ;;
	package) _arguments $global '(-m --manager)'{-m,--manager}'[package manager]:manager:' '(-i --interactive)'{-i,--interactive}'[pick packages from a list]' '1:subcommand:(add unmanaged)' '*:package:' ;;
	completion) _arguments '1:shell:(bash zsh fish)' ;;
	esac
}
compdef _pose pose
`

const fishCompletion = `# fish completion for pose
set -l cmds plan apply state edit add package completion version help
complete -c pose -f
complete -c pose -n "not __fish_seen_subcommand_from $cmds" -a "$cmds"
complete -c pose -s C -l dir -r -a "(__fish_complete_directories)" -d "configuration repository"
complete -c pose -l host -x -d "host overlay"
complete -c pose -l state -r -F -d "state file"
complete -c pose -l force-host -d "accept a state file written for another host"
complete -c pose -n "__fish_seen_subcommand_from plan" -l detailed-exitcode -d "exit 2 when there are changes"
complete -c pose -n "__fish_seen_subcommand_from plan apply" -l target -x -a "package dotfile service script (pose state list 2>/dev/null)" -d "only act on KEY"
complete -c pose -n "__fish_seen_subcommand_from apply" -s y -l yes -d "skip the confirmation prompt"
complete -c pose -n "__fish_seen_subcommand_from apply unmanaged" -s i -l interactive -d "pick from a list"
complete -c pose -n "__fish_seen_subcommand_from add" -F
complete -c pose -n "__fish_seen_subcommand_from state; and not __fish_seen_subcommand_from list rm" -a "list rm"
complete -c pose -n "__fish_seen_subcommand_from rm" -a "(pose state list 2>/dev/null)"
complete -c pose -n "__fish_seen_subcommand_from package; and not __fish_seen_subcommand_from add unmanaged" -a "add unmanaged"
complete -c pose -n "__fish_seen_subcommand_from package" -s m -l manager -x -d "package manager"
complete -c pose -n "__fish_seen_subcommand_from completion" -a "bash zsh fish"
`

func cmdCompletion(args []string) int {
	scripts := map[string]string{"bash": bashCompletion, "zsh": zshCompletion, "fish": fishCompletion}
	if len(args) != 1 || scripts[args[0]] == "" {
		fmt.Fprintln(os.Stderr, "usage: pose completion bash|zsh|fish")
		return 2
	}
	fmt.Print(scripts[args[0]])
	return 0
}
