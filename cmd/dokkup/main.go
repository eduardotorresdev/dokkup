// Command dokkup is a web interface for the day-to-day operation of a Dokku
// host.
//
// The same binary is the server and the command-line tool: installing dokkup
// means placing one file, and removing it means deleting one file and one
// directory.
package main

import (
	"os"

	"github.com/eduardotorresdev/dokkup/internal/cli"
)

// Set at build time by the linker; see the Makefile.
var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

func main() {
	os.Exit(cli.Run(cli.Env{
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		Build: cli.Build{
			Version: version,
			Commit:  commit,
			Date:    date,
		},
	}, os.Args[1:]))
}
