package cli

import (
	"context"
	"flag"
	"fmt"
	"sort"
	"strings"
)

// completionCommand writes a shell script that completes commands and flags.
//
// Static, and generated from the real tree rather than written by hand: the
// tree is walked at run time and the script it emits carries what it found, so
// a command added without a thought for completion is completed anyway. A
// hand-written list is a list that is wrong by the second release.
//
// **It completes nothing that needs the network.** Environment and project
// names would be the obvious next step and are deliberately absent: a TAB that
// makes an authenticated API call is a TAB that can be slow, can fail while
// somebody is mid-word, and can spend a token's request allowance -- the
// control plane allows one 600 a minute, and a person leaning on TAB is a
// plausible way to reach that. Completion is a text transformation on things
// this binary already knows.
func completionCommand() *Command {
	return &Command{
		Name:    "completion",
		Summary: "write a shell completion script",
		Usage:   "completion bash|zsh|fish",
		Long: `Completes command names, subcommands and flags. Writes the script to
stdout; where it goes from there is your shell's business:

    # bash — this session
    source <(vallic completion bash)
    # bash — every session
    vallic completion bash > ~/.local/share/bash-completion/completions/vallic

    # zsh, somewhere on $fpath
    vallic completion zsh > "${fpath[1]}/_vallic"

    # fish
    vallic completion fish > ~/.config/fish/completions/vallic.fish

It completes no names that live on the platform — not environments, not
projects. Those would need a request per keystroke against a control plane that
limits one token to six hundred a minute, so a person holding TAB could spend
an allowance their pipeline then needs. What it knows, it knows offline.`,
		Run: func(ctx context.Context, env *Env, args []string) error {
			if len(args) != 1 {
				return fmt.Errorf("name a shell: bash, zsh or fish")
			}

			tree := describeTree(Root(), env)

			switch args[0] {
			case "bash":
				fmt.Fprint(env.Out, bashCompletion(tree))
			case "zsh":
				fmt.Fprint(env.Out, zshCompletion(tree))
			case "fish":
				fmt.Fprint(env.Out, fishCompletion(tree))
			default:
				return fmt.Errorf("no completion for %q: bash, zsh or fish", args[0])
			}

			return nil
		},
	}
}

// completionNode is one command, flattened for a shell to read.
type completionNode struct {
	// Path is the words that reach it, without the binary name: "env", then
	// "env list". Empty for the root.
	Path []string

	// Subcommands are the names that may follow, sorted.
	Subcommands []string

	// Flags are the long flag names it accepts, sorted, without dashes and
	// including the globals every command takes.
	Flags []string
}

// describeTree flattens the command tree into what a shell script needs.
//
// Flags are collected by handing each command's own registrar a throwaway
// FlagSet and reading back what it registered, so this cannot disagree with
// what the command actually parses.
func describeTree(root *Command, env *Env) []completionNode {
	var nodes []completionNode

	var walk func(cmd *Command, path []string)

	walk = func(cmd *Command, path []string) {
		nodes = append(nodes, completionNode{
			Path:        path,
			Subcommands: childNames(cmd),
			Flags:       flagNames(cmd, env),
		})

		for _, child := range cmd.Children {
			walk(child, append(append([]string{}, path...), child.Name))
		}
	}

	walk(root, nil)

	return nodes
}

// childNames is a command's subcommands, aliases included, sorted.
//
// Aliases are offered because they are spellings that work: a completion that
// hid them would make a working command look like a typo.
func childNames(cmd *Command) []string {
	names := make([]string, 0, len(cmd.Children))

	for _, child := range cmd.Children {
		names = append(names, child.Name)
		names = append(names, child.Aliases...)
	}

	sort.Strings(names)

	return names
}

// flagNames is every long flag a command accepts, its own and the globals.
//
// --help is added because the dispatcher answers it everywhere without any
// command registering it, so reading the FlagSets alone would leave out the one
// flag that works on everything.
func flagNames(cmd *Command, env *Env) []string {
	fs := flag.NewFlagSet("completion", flag.ContinueOnError)
	fs.SetOutput(discard{})

	// Globals first, so a command that somehow registered the same name does
	// not end up listed twice.
	if env != nil {
		env.registerGlobals(fs)
	}

	if cmd.Flags != nil {
		cmd.Flags(fs)
	}

	seen := map[string]bool{"help": true}

	fs.VisitAll(func(f *flag.Flag) { seen[f.Name] = true })

	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, "--"+name)
	}

	sort.Strings(names)

	return names
}

// discard swallows a FlagSet's usage output.
type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// bashCompletion is a script that walks the words typed so far.
//
// One case statement keyed on the command path, which is the shape bash makes
// cheapest: no arrays of arrays, no subshell per keystroke.
func bashCompletion(nodes []completionNode) string {
	var b strings.Builder

	b.WriteString(`# vallic completion for bash. Generated by 'vallic completion bash'.
#
# Completes command names, subcommands and flags. Nothing here reaches the
# network: no environment or project names, deliberately, so TAB costs nothing
# and cannot spend an API allowance.

_vallic() {
    local cur words path key
    cur="${COMP_WORDS[COMP_CWORD]}"

    # The command path is the non-flag words before the cursor, minus the
    # binary. A flag's value is skipped with it, or "--project acme list" would
    # read as the path "acme list".
    path=()
    local i=1
    while (( i < COMP_CWORD )); do
        case "${COMP_WORDS[i]}" in
            -*) (( i++ )) ;;
            *) path+=("${COMP_WORDS[i]}") ;;
        esac
        (( i++ ))
    done

    key="${path[*]}"

    case "$key" in
`)

	for _, node := range nodes {
		words := append(append([]string{}, node.Subcommands...), node.Flags...)

		fmt.Fprintf(&b, "        %s)\n            COMPREPLY=($(compgen -W %q -- \"$cur\"))\n            return\n            ;;\n",
			bashPattern(node.Path), strings.Join(words, " "))
	}

	b.WriteString(`    esac
}

complete -F _vallic vallic
`)

	return b.String()
}

// bashPattern is the case label for a path.
//
// Quoted, always. A label of more than one word is a syntax error unquoted --
// `team list)` stops bash at `list` -- which a generator is uniquely good at
// producing and uniquely bad at noticing, because the script is only ever read
// by a shell.
func bashPattern(path []string) string {
	return `"` + strings.Join(path, " ") + `"`
}

// zshCompletion is a script keyed the same way, in zsh's own idiom.
func zshCompletion(nodes []completionNode) string {
	var b strings.Builder

	b.WriteString(`#compdef vallic
# vallic completion for zsh. Generated by 'vallic completion zsh'.
#
# Completes command names, subcommands and flags. Nothing here reaches the
# network: no environment or project names, deliberately, so TAB costs nothing
# and cannot spend an API allowance.

_vallic() {
    local -a path
    local key word

    # The non-flag words before the cursor, minus the binary. A flag's value is
    # skipped with it.
    local i=2
    while (( i < CURRENT )); do
        word="${words[i]}"
        if [[ "$word" == -* ]]; then
            (( i++ ))
        else
            path+=("$word")
        fi
        (( i++ ))
    done

    key="${(j: :)path}"

    case "$key" in
`)

	for _, node := range nodes {
		words := append(append([]string{}, node.Subcommands...), node.Flags...)

		fmt.Fprintf(&b, "        %s)\n            compadd -- %s\n            return\n            ;;\n",
			zshPattern(node.Path), strings.Join(words, " "))
	}

	b.WriteString(`    esac
}

_vallic "$@"
`)

	return b.String()
}

// zshPattern is the case label for a path. Quoted for bashPattern's reason.
func zshPattern(path []string) string {
	return `"` + strings.Join(path, " ") + `"`
}

// fishCompletion is a list of rules rather than a script.
//
// fish has no case statement to key on, so each command is a `complete` line
// guarded by `__fish_seen_subcommand_from`, which is the idiom and reads far
// better than reimplementing the walk.
func fishCompletion(nodes []completionNode) string {
	var b strings.Builder

	b.WriteString(`# vallic completion for fish. Generated by 'vallic completion fish'.
#
# Completes command names, subcommands and flags. Nothing here reaches the
# network: no environment or project names, deliberately, so TAB costs nothing
# and cannot spend an API allowance.

`)

	for _, node := range nodes {
		condition := "not __fish_seen_subcommand_from " + strings.Join(allFirstWords(nodes), " ")

		if len(node.Path) > 0 {
			// Every word of the path having been seen. Not exact -- fish has no
			// notion of order here -- and close enough that the wrong list is
			// only ever offered for a command nobody typed.
			condition = "__fish_seen_subcommand_from " + strings.Join(node.Path, "; and __fish_seen_subcommand_from ")
		}

		for _, word := range node.Subcommands {
			fmt.Fprintf(&b, "complete -c vallic -f -n %q -a %q\n", condition, word)
		}

		for _, word := range node.Flags {
			fmt.Fprintf(&b, "complete -c vallic -f -n %q -l %q\n", condition, strings.TrimPrefix(word, "--"))
		}
	}

	return b.String()
}

// allFirstWords is every top-level command name, for fish's "nothing typed yet"
// guard.
func allFirstWords(nodes []completionNode) []string {
	for _, node := range nodes {
		if len(node.Path) == 0 {
			return node.Subcommands
		}
	}

	return nil
}
