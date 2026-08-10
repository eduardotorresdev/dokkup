package cli

import (
	"fmt"

	"github.com/eduardotorresdev/dokkup/internal/hostpaths"
)

func runInstall(env Env, args []string) error {
	fs := newFlagSet(env, "install")
	domain := fs.String("domain", "", "domain to publish dokkup at (omit to use the host's IP address)")
	_ = fs.Bool("accept-self-signed", false, "in IP mode, generate a self-signed certificate without asking")

	if err := parseFlags(fs, args); err != nil {
		return err
	}

	printInstallPlan(env, *domain)
	return fmt.Errorf("%w: the installer is not built yet", ErrNotImplemented)
}

// printInstallPlan states what installation will do. It is printed before
// anything happens, and it names the same locations the uninstaller reads from
// hostpaths, so the two cannot drift apart.
func printInstallPlan(env Env, domain string) {
	printf(env.Stdout, "dokkup install will:\n\n")

	printf(env.Stdout, "  Create the system user %q in the %q group, with a sudoers rule\n",
		hostpaths.User, hostpaths.DokkuGroup)
	printf(env.Stdout, "  permitting exactly one program without a password: the dokku binary.\n\n")

	printf(env.Stdout, "  Write:\n")
	for _, path := range hostpaths.Owned() {
		printf(env.Stdout, "    %s\n", path)
	}
	printf(env.Stdout, "\n")

	printf(env.Stdout, "  Ensure the Let's Encrypt plugin is present on this host, if it is not\n")
	printf(env.Stdout, "  already. Removal will leave it in place: other apps depend on it for\n")
	printf(env.Stdout, "  certificate renewal.\n\n")

	if domain != "" {
		printf(env.Stdout, "  Check that %s resolves to this host, then publish dokkup there with a\n", domain)
		printf(env.Stdout, "  certificate. If DNS does not point here yet, you will be asked whether\n")
		printf(env.Stdout, "  to continue by IP address instead.\n\n")
	} else {
		printf(env.Stdout, "  Serve dokkup at this host's IP address. No certificate authority will\n")
		printf(env.Stdout, "  vouch for an IP, so you will be offered a self-signed certificate and\n")
		printf(env.Stdout, "  shown its fingerprint to check in the browser. In this mode dokkup\n")
		printf(env.Stdout, "  allows only the owner and warns on every screen. Leave it later with\n")
		printf(env.Stdout, "  'dokkup publish <domain>'.\n\n")
	}

	printf(env.Stdout, "  Print a single-use token, valid briefly, for creating the owner account\n")
	printf(env.Stdout, "  in the browser. It is revoked the moment it is used.\n\n")
}

func runPublish(env Env, args []string) error {
	fs := newFlagSet(env, "publish")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: dokkup publish <domain>")
	}

	domain := fs.Arg(0)
	printf(env.Stdout, "dokkup publish will check that %s resolves to this host, serve dokkup\n", domain)
	printf(env.Stdout, "there with a certificate, and leave IP mode.\n\n")

	return fmt.Errorf("%w: publishing is not built yet", ErrNotImplemented)
}

func runSetupToken(env Env, args []string) error {
	fs := newFlagSet(env, "setup-token")
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	printf(env.Stdout, "dokkup setup-token issues a single-use token for creating the owner.\n")
	printf(env.Stdout, "It refuses once an owner exists: without that condition it would be an\n")
	printf(env.Stdout, "unauthenticated route to taking over the account.\n\n")

	return fmt.Errorf("%w: token issuance is not built yet", ErrNotImplemented)
}
