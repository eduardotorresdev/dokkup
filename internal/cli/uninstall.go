package cli

import (
	"fmt"

	"github.com/eduardotorresdev/dokkup/internal/hostpaths"
)

func runUninstall(env Env, args []string) error {
	fs := newFlagSet(env, "uninstall")
	purge := fs.Bool("purge", false, "delete the data directory without asking")

	if err := parseFlags(fs, args); err != nil {
		return err
	}

	// The report is printed every time, as part of the command rather than
	// behind a flag. Someone removing dokkup should not have to have known to
	// ask what it is about to do.
	PrintRemovalReport(env, *purge)

	return fmt.Errorf("%w: the uninstaller is not built yet", ErrNotImplemented)
}

// PrintRemovalReport states exactly what removal takes and what it leaves.
//
// The second list matters more than the first. Uninstalling a management
// interface must not break the server it was managing, and saying so plainly is
// how an operator can trust that it will not.
func PrintRemovalReport(env Env, purge bool) {
	fmt.Fprintf(env.Stdout, "dokkup uninstall will REMOVE:\n\n")
	fmt.Fprintf(env.Stdout, "    %s\n", hostpaths.Unit)
	fmt.Fprintf(env.Stdout, "    %s\n", hostpaths.Sudoers)
	fmt.Fprintf(env.Stdout, "    %s\n", hostpaths.Binary)
	fmt.Fprintf(env.Stdout, "    system user and group %q\n", hostpaths.User)
	fmt.Fprintf(env.Stdout, "    the published vhost and its certificate, if dokkup created one\n")

	if purge {
		fmt.Fprintf(env.Stdout, "    %s  (--purge given: operators, audit trail and deploy history)\n",
			hostpaths.DataDir)
	} else {
		fmt.Fprintf(env.Stdout, "\n  and will ASK before removing:\n\n")
		fmt.Fprintf(env.Stdout, "    %s  (operators, audit trail, deploy history)\n", hostpaths.DataDir)
	}

	fmt.Fprintf(env.Stdout, "\n  It will NOT touch:\n\n")
	fmt.Fprintf(env.Stdout, "    your apps, and nothing they run\n")
	fmt.Fprintf(env.Stdout, "    volumes under %s -- your data stays\n", hostpaths.DokkuStorageRoot)
	fmt.Fprintf(env.Stdout, "    networks connecting your apps\n")
	fmt.Fprintf(env.Stdout, "    Dokku plugins, including the Let's Encrypt plugin the installer\n")
	fmt.Fprintf(env.Stdout, "      ensured was present: removing it would stop certificate renewal\n")
	fmt.Fprintf(env.Stdout, "      for apps that have nothing to do with dokkup\n")
	fmt.Fprintf(env.Stdout, "    registry credentials held by Docker\n")
	fmt.Fprintf(env.Stdout, "    Dokku itself\n\n")

	fmt.Fprintf(env.Stdout, "  You will be asked to authenticate again before anything is removed:\n")
	fmt.Fprintf(env.Stdout, "  your password, or the host's name if you are already root.\n\n")
}
