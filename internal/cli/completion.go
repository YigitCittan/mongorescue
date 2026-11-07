package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
)

const completionUsage = `Usage: mongorescue completion bash|zsh|fish|powershell

Prints a completion script for the commands and flags of mongorescue to stdout.
The script is static: it needs no running instance. See docs/cli.md#shell-completion
for how to install it in each shell.
`

// completionShells lists the shells "mongorescue completion" knows, with the
// generator of each.
var completionShells = []struct {
	name string
	gen  func(*strings.Builder, []commandInfo)
}{
	{"bash", genBash},
	{"zsh", genZsh},
	{"fish", genFish},
	{"powershell", genPowerShell},
}

// flagInfo describes a flag of a command for the completion scripts.
type flagInfo struct {
	name  string
	usage string
	// value is true when the flag takes a value (it is not a boolean).
	value bool
	// file is true when the value is a file name.
	file bool
}

// commandInfo describes a command for the completion scripts.
type commandInfo struct {
	name    string
	summary string
	flags   []flagInfo
	// args lists the values the first argument can take, when it is a closed set.
	args []string
}

// commandTable builds what the completion scripts offer from the CLI's own
// definitions: the command list (commandDocs), the flag sets of the commands (run
// up to the point where they are parsed, with no network access) and the resources
// of list. It is the single source of truth: a command or flag added there shows up
// in the scripts.
func (a *App) commandTable() []commandInfo {
	funcs := commandFuncs()
	var table []commandInfo
	for _, d := range commandDocs {
		ci := commandInfo{name: d.name, summary: d.summary}
		switch d.name {
		case "completion":
			for _, sh := range completionShells {
				ci.args = append(ci.args, sh.name)
			}
		case "help":
			for _, other := range commandDocs {
				ci.args = append(ci.args, other.name)
			}
		default:
			ci.flags = a.inspectFlags(funcs[d.name])
		}
		if d.name == "list" {
			for resource := range listResources {
				ci.args = append(ci.args, resource)
			}
			slices.Sort(ci.args)
		}
		if d.name != "completion" && d.name != "help" {
			ci.flags = append(ci.flags, flagInfo{name: "help", usage: "Show the flags of this command"})
		}
		table = append(table, ci)
	}
	for _, e := range a.ExtraCommands {
		table = append(table, commandInfo{name: e.Name, summary: e.Summary})
	}
	return table
}

// inspectFlags returns the flags a command defines, by running it with a session
// that stops at the parse of the flags.
func (a *App) inspectFlags(fn command) []flagInfo {
	var flags []flagInfo
	s := &session{app: a, stdout: io.Discard, stderr: io.Discard, getenv: func(string) string { return "" }}
	s.inspect = func(fs *flag.FlagSet) {
		fs.VisitAll(func(f *flag.Flag) {
			bf, isBool := f.Value.(interface{ IsBoolFlag() bool })
			flags = append(flags, flagInfo{
				name: f.Name, usage: shortDescription(f.Usage),
				value: !isBool || !bf.IsBoolFlag(), file: strings.HasSuffix(f.Name, "-file"),
			})
		})
	}
	_ = fn(context.Background(), s, nil)
	return flags
}

// shortDescription is text on one line, cut at about 100 characters, for the
// descriptions the shells show next to a candidate.
func shortDescription(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	const limit = 100
	if len(text) <= limit {
		return text
	}
	cut := strings.LastIndex(text[:limit-3], " ")
	if cut < 0 {
		cut = limit - 3
	}
	return text[:cut] + "..."
}

// runCompletion implements "mongorescue completion".
func runCompletion(_ context.Context, s *session, args []string) error {
	fs := flag.NewFlagSet("mongorescue completion", flag.ContinueOnError)
	fs.SetOutput(s.stderr)
	fs.Usage = func() { fmt.Fprint(s.stderr, completionUsage) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return errHelp
		}
		return fmt.Errorf("%w: %w", ErrUsage, err)
	}
	pos := fs.Args()
	if len(pos) != 1 {
		return usageErrorf("completion takes one shell: bash, zsh, fish or powershell")
	}
	for _, sh := range completionShells {
		if sh.name == pos[0] {
			var b strings.Builder
			sh.gen(&b, s.app.commandTable())
			_, err := io.WriteString(s.stdout, b.String())
			return err
		}
	}
	return usageErrorf("unknown shell %q: want bash, zsh, fish or powershell", pos[0])
}

// commandNamesOf returns the command names of table.
func commandNamesOf(table []commandInfo) []string {
	names := make([]string, len(table))
	for i, c := range table {
		names[i] = c.name
	}
	return names
}

// flagWords returns "--name" for each flag, optionally only the ones that take a
// value (valueOnly) or a file (fileOnly).
func flagWords(flags []flagInfo, valueOnly, fileOnly bool) []string {
	var out []string
	for _, f := range flags {
		if (valueOnly && !f.value) || (fileOnly && !f.file) {
			continue
		}
		out = append(out, "--"+f.name)
	}
	return out
}

// genBash writes the completion script for bash.
func genBash(b *strings.Builder, table []commandInfo) {
	b.WriteString("# bash completion for mongorescue; generated by \"mongorescue completion bash\".\n")
	b.WriteString("# Load it with: source <(mongorescue completion bash)\n\n")
	b.WriteString("_mongorescue() {\n")
	b.WriteString("    local cur prev cmd i flags words valueflags fileflags oldifs\n")
	b.WriteString("    COMPREPLY=()\n")
	b.WriteString("    cur=\"${COMP_WORDS[COMP_CWORD]}\"\n")
	b.WriteString("    prev=\"\"\n")
	b.WriteString("    if (( COMP_CWORD > 0 )); then prev=\"${COMP_WORDS[COMP_CWORD-1]}\"; fi\n")
	b.WriteString("    # --flag=value: bash splits at the = (it is a word of its own).\n")
	b.WriteString("    if [[ $cur == \"=\" ]]; then\n        cur=\"\"\n")
	b.WriteString("    elif [[ $prev == \"=\" && COMP_CWORD -gt 1 ]]; then\n        prev=\"${COMP_WORDS[COMP_CWORD-2]}\"\n    fi\n")
	b.WriteString("    cmd=\"\"\n")
	b.WriteString("    for (( i = 1; i < COMP_CWORD; i++ )); do\n")
	fmt.Fprintf(b, "        case \"${COMP_WORDS[i]}\" in\n            %s)\n                cmd=\"${COMP_WORDS[i]}\"\n                break\n                ;;\n        esac\n    done\n",
		strings.Join(commandNamesOf(table), "|"))
	fmt.Fprintf(b, "    if [[ -z $cmd ]]; then\n        COMPREPLY=( $(compgen -W %q -- \"$cur\") )\n        return 0\n    fi\n", strings.Join(commandNamesOf(table), " "))
	b.WriteString("    flags=\"\" words=\"\" valueflags=\" \" fileflags=\" \"\n")
	b.WriteString("    case \"$cmd\" in\n")
	for _, c := range table {
		if len(c.flags) == 0 && len(c.args) == 0 {
			continue
		}
		fmt.Fprintf(b, "        %s)\n", c.name)
		fmt.Fprintf(b, "            flags=%q\n", strings.Join(flagWords(c.flags, false, false), " "))
		fmt.Fprintf(b, "            valueflags=%q\n", " "+strings.Join(flagWords(c.flags, true, false), " ")+" ")
		fmt.Fprintf(b, "            fileflags=%q\n", " "+strings.Join(flagWords(c.flags, true, true), " ")+" ")
		fmt.Fprintf(b, "            words=%q\n", strings.Join(c.args, " "))
		b.WriteString("            ;;\n")
	}
	b.WriteString("    esac\n")
	b.WriteString("    if [[ $fileflags == *\" $prev \"* ]]; then\n")
	b.WriteString("        oldifs=$IFS\n        IFS=$'\\n'\n        COMPREPLY=( $(compgen -f -- \"$cur\") )\n        IFS=$oldifs\n        return 0\n    fi\n")
	b.WriteString("    if [[ $valueflags == *\" $prev \"* ]]; then\n        return 0\n    fi\n")
	b.WriteString("    if [[ $cur == -* ]]; then\n        COMPREPLY=( $(compgen -W \"$flags\" -- \"$cur\") )\n        return 0\n    fi\n")
	b.WriteString("    COMPREPLY=( $(compgen -W \"$words\" -- \"$cur\") )\n}\n\n")
	b.WriteString("complete -F _mongorescue mongorescue mongorescue.exe\n")
}

// zshQuote single-quotes text for zsh.
func zshQuote(text string) string {
	return "'" + strings.ReplaceAll(text, "'", `'\''`) + "'"
}

// zshSpec escapes text for the description of an _arguments option.
func zshSpec(text string) string {
	return strings.ReplaceAll(strings.ReplaceAll(text, `\`, `\\`), "]", `\]`)
}

// genZsh writes the completion script for zsh.
func genZsh(b *strings.Builder, table []commandInfo) {
	b.WriteString("#compdef mongorescue\n")
	b.WriteString("# zsh completion for mongorescue; generated by \"mongorescue completion zsh\".\n")
	b.WriteString("# Load it with: source <(mongorescue completion zsh)   (after compinit)\n\n")
	b.WriteString("_mongorescue() {\n")
	b.WriteString("  local context state state_descr line\n  typeset -A opt_args\n  local -a commands\n  commands=(\n")
	for _, c := range table {
		fmt.Fprintf(b, "    %s\n", zshQuote(c.name+":"+c.summary))
	}
	b.WriteString("  )\n")
	b.WriteString("  _arguments -C \\\n    '1: :->command' \\\n    '*:: :->args'\n")
	b.WriteString("  case $state in\n    command)\n      _describe -t commands 'mongorescue command' commands\n      ;;\n    args)\n      case $line[1] in\n")
	for _, c := range table {
		if len(c.flags) == 0 && len(c.args) == 0 {
			continue
		}
		fmt.Fprintf(b, "        %s)\n          _arguments \\\n", c.name)
		for _, f := range c.flags {
			spec := "--" + f.name
			switch {
			case f.file:
				spec += "=[" + zshSpec(f.usage) + "]:file:_files"
			case f.value:
				spec += "=[" + zshSpec(f.usage) + "]:value: "
			default:
				spec += "[" + zshSpec(f.usage) + "]"
			}
			fmt.Fprintf(b, "            %s \\\n", zshQuote(spec))
		}
		switch {
		case len(c.args) > 0:
			fmt.Fprintf(b, "            %s\n", zshQuote("1:argument:("+strings.Join(c.args, " ")+")"))
		default:
			b.WriteString("            '*:argument: '\n")
		}
		b.WriteString("          ;;\n")
	}
	b.WriteString("      esac\n      ;;\n  esac\n}\n\n")
	b.WriteString("if [ \"$funcstack[1]\" = \"_mongorescue\" ]; then\n  _mongorescue \"$@\"\nelse\n  compdef _mongorescue mongorescue\nfi\n")
}

// fishQuote single-quotes text for fish.
func fishQuote(text string) string {
	return "'" + strings.ReplaceAll(strings.ReplaceAll(text, `\`, `\\`), "'", `\'`) + "'"
}

// genFish writes the completion script for fish.
func genFish(b *strings.Builder, table []commandInfo) {
	b.WriteString("# fish completion for mongorescue; generated by \"mongorescue completion fish\".\n")
	b.WriteString("# Load it with: mongorescue completion fish | source\n\n")
	b.WriteString("complete -c mongorescue -f\n")
	for _, c := range table {
		fmt.Fprintf(b, "complete -c mongorescue -n '__fish_use_subcommand' -a %s -d %s\n", c.name, fishQuote(c.summary))
	}
	for _, c := range table {
		seen := "__fish_seen_subcommand_from " + c.name
		for _, f := range c.flags {
			line := fmt.Sprintf("complete -c mongorescue -n %s -l %s", fishQuote(seen), f.name)
			switch {
			case f.file:
				line += " -r -F"
			case f.value:
				line += " -r"
			}
			fmt.Fprintf(b, "%s -d %s\n", line, fishQuote(f.usage))
		}
		if len(c.args) > 0 {
			// The command's own name is not among the words that end the suggestions
			// ("help" is one of the commands help completes).
			others := slices.DeleteFunc(slices.Clone(c.args), func(a string) bool { return a == c.name })
			cond := seen + "; and not __fish_seen_subcommand_from " + strings.Join(others, " ")
			fmt.Fprintf(b, "complete -c mongorescue -n %s -a %s\n", fishQuote(cond), fishQuote(strings.Join(c.args, " ")))
		}
	}
}

// psQuote single-quotes text for PowerShell.
func psQuote(text string) string {
	return "'" + strings.ReplaceAll(text, "'", "''") + "'"
}

// genPowerShell writes the completion script for PowerShell (Windows PowerShell 5.1
// and PowerShell 7).
func genPowerShell(b *strings.Builder, table []commandInfo) {
	b.WriteString("# PowerShell completion for mongorescue; generated by \"mongorescue completion powershell\".\n")
	b.WriteString("# Load it with: mongorescue completion powershell | Out-String | Invoke-Expression\n\n")
	b.WriteString("Register-ArgumentCompleter -Native -CommandName 'mongorescue', 'mongorescue.exe' -ScriptBlock {\n")
	b.WriteString("    param($wordToComplete, $commandAst, $cursorPosition)\n\n")
	b.WriteString("    $commands = [ordered]@{\n")
	for _, c := range table {
		fmt.Fprintf(b, "        %s = %s\n", psQuote(c.name), psQuote(c.summary))
	}
	b.WriteString("    }\n")
	b.WriteString("    $flags = @{\n")
	for _, c := range table {
		if len(c.flags) == 0 {
			continue
		}
		fmt.Fprintf(b, "        %s = [ordered]@{\n", psQuote(c.name))
		for _, f := range c.flags {
			fmt.Fprintf(b, "            %s = %s\n", psQuote("--"+f.name), psQuote(f.usage))
		}
		b.WriteString("        }\n")
	}
	b.WriteString("    }\n")
	b.WriteString("    $valueFlags = @{\n")
	for _, c := range table {
		if v := flagWords(c.flags, true, false); len(v) > 0 {
			quoted := make([]string, len(v))
			for i, w := range v {
				quoted[i] = psQuote(w)
			}
			fmt.Fprintf(b, "        %s = @(%s)\n", psQuote(c.name), strings.Join(quoted, ", "))
		}
	}
	b.WriteString("    }\n")
	b.WriteString("    $arguments = @{\n")
	for _, c := range table {
		if len(c.args) > 0 {
			quoted := make([]string, len(c.args))
			for i, w := range c.args {
				quoted[i] = psQuote(w)
			}
			fmt.Fprintf(b, "        %s = @(%s)\n", psQuote(c.name), strings.Join(quoted, ", "))
		}
	}
	b.WriteString("    }\n\n")
	b.WriteString(`    $elements = @($commandAst.CommandElements | ForEach-Object { $_.ToString() })
    # The word being completed is the last element unless it is empty.
    $limit = $elements.Count
    if ($wordToComplete) { $limit-- }
    $command = $null
    for ($i = 1; $i -lt $limit; $i++) {
        if ($commands.Contains($elements[$i])) { $command = $elements[$i]; break }
    }
    $results = @()
    if (-not $command) {
        foreach ($name in $commands.Keys) { if ($name -like "$wordToComplete*") { $results += @{ Text = $name; Tip = $commands[$name] } } }
    } else {
        $previous = if ($limit -gt 0) { $elements[$limit - 1] } else { '' }
        if ($valueFlags.ContainsKey($command) -and ($valueFlags[$command] -contains $previous)) {
            # A flag value: no candidates, so PowerShell completes file names.
            return
        }
        if ($wordToComplete -like '-*') {
            if ($flags.ContainsKey($command)) {
                foreach ($name in $flags[$command].Keys) { if ($name -like "$wordToComplete*") { $results += @{ Text = $name; Tip = $flags[$command][$name] } } }
            }
        } elseif ($arguments.ContainsKey($command)) {
            foreach ($name in $arguments[$command]) { if ($name -like "$wordToComplete*") { $results += @{ Text = $name; Tip = $name } } }
        }
    }
    $results | ForEach-Object {
        [System.Management.Automation.CompletionResult]::new($_.Text, $_.Text, 'ParameterValue', $_.Tip)
    }
}
`)
}
