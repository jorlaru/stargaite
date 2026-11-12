// Command stargaite is the proxy and its command line.
package main

import (
	"io"
	"os"

	"github.com/jorlaru/stargaite/internal/cli"
)

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

// run starts the proxy for `serve` and hands every other command to the CLI.
func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "serve" {
		return serve(args[1:], getenv, stdout, stderr)
	}
	return cli.Main(args, getenv, stdout, stderr)
}
