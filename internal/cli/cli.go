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
	// Stdin is where a question is answered. Installation asks whether to
	// continue past a domain that does not resolve here, and removal asks the
	// operator to prove they meant it; both must be answerable by a test and by
	// a pipe, so neither reads os.Stdin directly.
	Stdin io.Reader

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
	if env.Stdin == nil {
		env.Stdin = os.Stdin
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
	case "update":
		err = runUpdate(env, rest)
	case "publish":
		err = runPublish(env, rest)
	case "setup-token":
		err = runSetupToken(env, rest)
	case "version", "--version", "-v":
		runVersion(env)
		return 0
	case "help", "--help", "-h":
		usage(env.Stdout)
		return 0
	default:
		printf(env.Stderr, "dokkup: unknown command %q\n\n", name)
		usage(env.Stderr)
		return 2
	}

	if err != nil && !errors.Is(err, errFlagHelp) {
		printf(env.Stderr, "dokkup %s: %v\n", name, err)
	}
	return exitCode(err)
}

// exitCode maps a subcommand's error to what the shell sees.
//
// The codes are a contract with whatever is calling dokkup unattended, so they
// live in one function that can be read and tested on its own rather than being
// spread across the commands that produce them.
func exitCode(err error) int {
	switch {
	case err == nil, errors.Is(err, errFlagHelp):
		return 0
	case errors.Is(err, ErrNotImplemented):
		return 3
	// 4 and 5 are told apart because an unattended caller needs them apart:
	// "you are fine, on the old version" and "this host may have no working
	// binary" deserve different pages in the middle of the night.
	case errors.Is(err, ErrRolledBack):
		return 4
	case errors.Is(err, ErrRollbackFailed):
		return 5
	default:
		return 1
	}
}

func usage(w io.Writer) {
	printf(w, `dokkup -- a web interface for a Dokku host

Usage:
  dokkup <command> [flags]

Commands:
  install       Install dokkup on this Dokku host
  update        Move to a newer version, rolling back if it does not come up
  uninstall     Remove dokkup, reporting exactly what goes and what stays
  publish       Serve dokkup at a domain with a certificate
  setup-token   Issue a token to create the owner, while no owner exists
  serve         Run the server (this is what the systemd unit invokes)
  version       Print version information

Run 'dokkup <command> --help' for the flags of a command.

Exit codes:
  0  It worked. 'update --check' also exits 0 when an update is waiting: that
     is news rather than a failure.
  1  It did not work.
  2  The command was used wrongly.
  3  The command exists but is not built yet.
  4  'update' installed a version that did not come up, and put the previous
     one back. The host is serving the version it was serving before.
  5  'update' could not restore a working service. This host needs attention.
`)
}

func runVersion(env Env) {
	printf(env.Stdout, "dokkup %s (commit %s, built %s)\n",
		env.Build.Version, env.Build.Commit, env.Build.Date)
}

// printf writes to one of the CLI's own output streams. A failure to write to
// the terminal cannot be reported anywhere the operator would see it, so the
// error is dropped here, once, instead of at every call site.
func printf(w io.Writer, format string, a ...any) {
	_, _ = fmt.Fprintf(w, format, a...)
}
