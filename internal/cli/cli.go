// Package cli implements dokkup's subcommands.
//
// The CLI and the server are the same binary: installing dokkup means placing
// one file, and removing it means deleting one file and one directory. See
// docs/adr/0004-single-go-binary-with-embedded-csr-frontend.md.
package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// ErrNotImplemented marks a subcommand that exists in the scaffold but does not
// do its job yet. It is a distinct error so that nothing mistakes a stub for a
// completed operation.
var ErrNotImplemented = errors.New("not implemented yet")

// Build describes the running binary, for `dokkup version`.
type Build struct {
	Version string
	Commit  string
	Date    string
}

// Env is everything a subcommand needs from the outside world, so that tests can
// supply their own.
type Env struct {
	Stdout io.Writer
	Stderr io.Writer
	Build  Build
}

// Run dispatches args (excluding the program name) and returns the exit code.
func Run(env Env, args []string) int {
	if env.Stdout == nil {
		env.Stdout = os.Stdout
	}
	if env.Stderr == nil {
		env.Stderr = os.Stderr
	}

	if len(args) == 0 {
		usage(env.Stderr)
		return 2
	}

	name, rest := args[0], args[1:]

	var err error
	switch name {
	case "serve":
		err = runServe(env, rest)
	case "install":
		err = runInstall(env, rest)
	case "uninstall":
		err = runUninstall(env, rest)
	case "publish":
		err = runPublish(env, rest)
	case "setup-token":
		err = runSetupToken(env, rest)
	case "version", "--version", "-v":
		err = runVersion(env)
	case "help", "--help", "-h":
		usage(env.Stdout)
		return 0
	default:
		fmt.Fprintf(env.Stderr, "dokkup: unknown command %q\n\n", name)
		usage(env.Stderr)
		return 2
	}

	switch {
	case err == nil:
		return 0
	case errors.Is(err, flagHelp):
		return 0
	case errors.Is(err, ErrNotImplemented):
		fmt.Fprintf(env.Stderr, "dokkup %s: %v\n", name, err)
		return 3
	default:
		fmt.Fprintf(env.Stderr, "dokkup %s: %v\n", name, err)
		return 1
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `dokkup -- a web interface for a Dokku host

Usage:
  dokkup <command> [flags]

Commands:
  install       Install dokkup on this Dokku host
  uninstall     Remove dokkup, reporting exactly what goes and what stays
  publish       Serve dokkup at a domain with a certificate
  setup-token   Issue a token to create the owner, while no owner exists
  serve         Run the server (this is what the systemd unit invokes)
  version       Print version information

Run 'dokkup <command> --help' for the flags of a command.
`)
}

func runVersion(env Env) error {
	fmt.Fprintf(env.Stdout, "dokkup %s (commit %s, built %s)\n",
		env.Build.Version, env.Build.Commit, env.Build.Date)
	return nil
}
