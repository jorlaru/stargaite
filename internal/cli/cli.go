// Package cli is the command line: a command table, one flag set per command
// and deterministic exit codes. No command ever reads standard input.
package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
)

// Version is the version string. Release builds set it at link time.
var Version = "dev"

// Exit codes: success, failure, wrong usage.
const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

type input struct {
	json bool
}

type command struct {
	words []string // leading words: {"providers", "add"}
	args  []string // positional arguments, all required
	local func(in input, stdout io.Writer) error
}

var commands = []command{
	{words: []string{"version"}, local: version},
}

// Main runs one command and returns the exit code of the process.
func Main(args []string, _ func(string) string, stdout, stderr io.Writer) int {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "-help" || args[0] == "--help") {
		usage(stdout)
		return exitOK
	}
	cmd, rest := match(args)
	if cmd == nil {
		if len(args) == 0 {
			say(stderr, "stargaite: no command given\n")
		} else {
			say(stderr, "stargaite: unknown command %q\n", args[0])
		}
		usage(stderr)
		return exitUsage
	}

	name := "stargaite " + strings.Join(cmd.words, " ")
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	var in input
	fs.BoolVar(&in.json, "json", false, "print JSON")

	pos, err := parse(fs, rest)
	switch {
	case errors.Is(err, flag.ErrHelp):
		say(stdout, "usage: %s [flags]\n", strings.Join(append([]string{name}, cmd.args...), " "))
		fs.SetOutput(stdout)
		fs.PrintDefaults()
		return exitOK
	case err != nil:
		say(stderr, "%s: %v\n", name, err)
		return exitUsage
	case len(pos) != len(cmd.args):
		say(stderr, "%s: takes %d positional argument(s), got %d\n", name, len(cmd.args), len(pos))
		return exitUsage
	}

	if err := cmd.local(in, stdout); err != nil {
		say(stderr, "%s: %v\n", name, err)
		return exitFailure
	}
	return exitOK
}

// match returns the command whose words lead args, and the arguments after them.
func match(args []string) (*command, []string) {
	for i := range commands {
		n := len(commands[i].words)
		if len(args) >= n && slices.Equal(commands[i].words, args[:n]) {
			return &commands[i], args[n:]
		}
	}
	return nil, nil
}

// parse reads flags written before, between and after the positional
// arguments, which it returns: the flag package stops at the first positional.
// The first "--" ends flag parsing and is never taken as the value of a flag;
// "--flag=--" passes that literal.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos, tail []string
	if i := slices.Index(args, "--"); i >= 0 {
		args, tail = args[:i], args[i+1:]
	}
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if args = fs.Args(); len(args) > 0 {
			pos, args = append(pos, args[0]), args[1:]
		}
	}
	return append(pos, tail...), nil
}

func version(in input, stdout io.Writer) error {
	if in.json {
		return json.NewEncoder(stdout).Encode(map[string]string{"version": Version})
	}
	_, err := fmt.Fprintln(stdout, Version)
	return err
}

func usage(w io.Writer) {
	say(w, "usage: stargaite <command> [arguments] [flags]\n\ncommands:\n  serve\n")
	for _, c := range commands {
		say(w, "  %s\n", strings.Join(append(slices.Clone(c.words), c.args...), " "))
	}
}

// say writes a message for a person. A failed write has nowhere to be reported.
func say(w io.Writer, format string, a ...any) {
	_, _ = fmt.Fprintf(w, format, a...)
}
