package cli

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"slices"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		pos     []string
		json    bool
		label   string
		wantErr bool
	}{
		{name: "no arguments"},
		{name: "flags before", args: []string{"--json", "--label", "x", "a", "b"}, pos: []string{"a", "b"}, json: true, label: "x"},
		{name: "flags after", args: []string{"a", "b", "--json", "--label=x"}, pos: []string{"a", "b"}, json: true, label: "x"},
		{name: "flags between", args: []string{"a", "--json", "b", "--label", "x"}, pos: []string{"a", "b"}, json: true, label: "x"},
		{name: "single dash flags", args: []string{"a", "-json", "-label", "x"}, pos: []string{"a"}, json: true, label: "x"},
		{name: "terminator", args: []string{"a", "--", "--json", "b"}, pos: []string{"a", "--json", "b"}},
		{name: "only the first terminator ends parsing", args: []string{"--", "a", "--", "b"}, pos: []string{"a", "--", "b"}},
		{name: "terminator is never the value of a flag", args: []string{"--label", "--", "a"}, wantErr: true},
		{name: "terminator as an explicit value", args: []string{"--label=--", "a"}, pos: []string{"a"}, label: "--"},
		{name: "a lone dash is positional", args: []string{"-", "--json"}, pos: []string{"-"}, json: true},
		{name: "unknown flag after a positional", args: []string{"a", "--nope"}, wantErr: true},
		{name: "flag without its value", args: []string{"a", "--label"}, wantErr: true},
		{name: "boolean with a wrong value", args: []string{"--json=maybe"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			asJSON := fs.Bool("json", false, "")
			label := fs.String("label", "", "")
			pos, err := parse(fs, tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("no error; positionals %q", pos)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(pos, tc.pos) || *asJSON != tc.json || *label != tc.label {
				t.Errorf("positionals %q json %v label %q, want %q %v %q", pos, *asJSON, *label, tc.pos, tc.json, tc.label)
			}
		})
	}
}

func TestMainExitCodes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   []string
		code   int
		stdout string // exact when not empty
	}{
		{name: "version", args: []string{"version"}, code: 0, stdout: Version + "\n"},
		{name: "version as JSON", args: []string{"version", "--json"}, code: 0, stdout: `{"version":"` + Version + `"}` + "\n"},
		{name: "top-level help", args: []string{"--help"}, code: 0},
		{name: "top-level -h", args: []string{"-h"}, code: 0},
		{name: "top-level -help", args: []string{"-help"}, code: 0},
		{name: "command help", args: []string{"version", "--help"}, code: 0},
		{name: "no command", code: 2},
		{name: "unknown command", args: []string{"nope"}, code: 2},
		{name: "a command not shipped yet", args: []string{"usage", "show"}, code: 2},
		{name: "flag before the command", args: []string{"--json", "version"}, code: 2},
		{name: "unknown flag", args: []string{"version", "--nope"}, code: 2},
		{name: "wrong flag value", args: []string{"version", "--json=maybe"}, code: 2},
		{name: "unexpected positional", args: []string{"version", "extra"}, code: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := Main(tc.args, func(string) string { return "" }, &stdout, &stderr)
			if code != tc.code {
				t.Fatalf("exit %d, want %d; stderr %q", code, tc.code, &stderr)
			}
			if tc.stdout != "" && stdout.String() != tc.stdout {
				t.Errorf("stdout %q, want %q", &stdout, tc.stdout)
			}
			if code == 0 && (stdout.Len() == 0 || stderr.Len() != 0) {
				t.Errorf("success must write to stdout only; stdout %q stderr %q", &stdout, &stderr)
			}
			if code == 2 && (stdout.Len() != 0 || stderr.Len() == 0) {
				t.Errorf("wrong usage must write to stderr only; stdout %q stderr %q", &stdout, &stderr)
			}
		})
	}
}

func TestUsageListsEveryCommand(t *testing.T) {
	var stdout bytes.Buffer
	Main([]string{"--help"}, nil, &stdout, io.Discard)
	for _, want := range append([]string{"serve"}, commandNames()...) {
		if !strings.Contains(stdout.String(), "\n  "+want) {
			t.Errorf("usage does not list %q:\n%s", want, &stdout)
		}
	}
}

func commandNames() []string {
	var names []string
	for _, c := range commands {
		names = append(names, strings.Join(c.words, " "))
	}
	return names
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("closed") }

func TestFailureExitsOne(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"version", "--json"}} {
		var stderr bytes.Buffer
		if code := Main(args, nil, failingWriter{}, &stderr); code != 1 {
			t.Errorf("%q: exit %d, want 1", args, code)
		}
		if !strings.Contains(stderr.String(), "closed") {
			t.Errorf("%q: stderr %q does not carry the cause", args, &stderr)
		}
	}
}
