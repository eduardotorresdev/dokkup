package service_test

import (
	"strings"
	"testing"

	"github.com/eduardotorresdev/dokkup/internal/hostpaths"
	"github.com/eduardotorresdev/dokkup/internal/service"
)

// The unit is the one definition of how dokkup runs, so it has to name the
// paths hostpaths owns rather than repeating them as literals -- otherwise
// moving one moves the installer and leaves the unit pointing at the old place.
func TestTheUnitRunsTheInstalledBinaryAsTheInstalledUser(t *testing.T) {
	t.Parallel()

	unit := service.UnitFile(service.UnitConfig{})

	for _, want := range []string{
		"ExecStart=" + hostpaths.Binary + " serve",
		"User=" + hostpaths.User,
		"Group=" + hostpaths.Group,
		"ReadWritePaths=" + hostpaths.DataDir,
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("the unit is missing %q:\n%s", want, unit)
		}
	}
}

// ADR-0005 is a promise about how the service runs. A hardening line dropped by
// accident would not fail anything else, so it is asserted here.
func TestTheUnitKeepsTheHardeningADR0005Promises(t *testing.T) {
	t.Parallel()

	unit := service.UnitFile(service.UnitConfig{})

	for _, want := range []string{
		"ProtectSystem=full",
		"PrivateTmp=yes",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("the unit no longer sets %q:\n%s", want, unit)
		}
	}
}

// The hardening ADR-0005 originally listed that the unit must NOT carry.
//
// Every sandbox in this unit is inherited by the dokku process the service runs,
// and each of these breaks it while breaking nothing a test would otherwise
// notice: `dokku version`, the only command /api/health invokes, keeps answering
// happily. Measured against a real Dokku 0.38.7, in order: sudo refuses to run
// under NoNewPrivileges; ProtectHome hides DOKKU_ROOT so every app listing comes
// back empty on a host that has apps; ProtectSystem=strict makes /var/lib/dokku
// read-only so every write fails. See ADR-0012.
func TestTheUnitOmitsTheHardeningThatWouldSilentlyBreakDokku(t *testing.T) {
	t.Parallel()

	// Directives, not words: the unit explains each absence in a comment, and
	// those comments are the reason nobody adds them back.
	banned := map[string]string{
		"NoNewPrivileges":      "sudo cannot run under it, so dokkup never reaches Dokku",
		"ProtectHome":          "it hides DOKKU_ROOT, so every app listing comes back empty",
		"ProtectSystem=strict": "it makes /var/lib/dokku read-only, so every write fails",
	}

	for line := range strings.SplitSeq(service.UnitFile(service.UnitConfig{}), "\n") {
		directive := strings.TrimSpace(line)
		for prefix, why := range banned {
			if strings.HasPrefix(directive, prefix) {
				t.Errorf("the unit sets %q: %s", directive, why)
			}
		}
	}
}

func TestTheUnitFallsBackToTheSameDefaultsServeWouldHaveUsed(t *testing.T) {
	t.Parallel()

	unit := service.UnitFile(service.UnitConfig{})

	if !strings.Contains(unit, "--listen "+service.DefaultListen) {
		t.Errorf("the unit does not carry the default address:\n%s", unit)
	}
	if !strings.Contains(unit, "--mode "+service.DefaultMode) {
		t.Errorf("the unit does not carry the default mode:\n%s", unit)
	}
}

// An installation that has no domain says so by saying nothing, so that
// `systemctl cat dokkup` stays readable as the record of how this host was set
// up.
func TestTheUnitOfAnInstallationWithNoDomainMentionsNoneOfTheCertificateFlags(t *testing.T) {
	t.Parallel()

	unit := service.UnitFile(service.UnitConfig{})

	for _, unwanted := range []string{
		"--domain", "--manage-certificate", "--acme-email", "--acme-directory",
	} {
		if strings.Contains(unit, unwanted) {
			t.Errorf("the unit carries %s on an installation that has no domain:\n%s", unwanted, unit)
		}
	}
}

// Renewal happens because the unit asked for it. An ExecStart that lost
// --manage-certificate would go on serving the placeholder the installer wrote,
// for ten years, with nothing failing anywhere.
func TestTheUnitAsksTheServiceForTheCertificateWhenInstallationChoseTo(t *testing.T) {
	t.Parallel()

	unit := service.UnitFile(service.UnitConfig{
		Mode:              "published",
		Domain:            "dokkup.example.com",
		ManageCertificate: true,
		ACMEEmail:         "operator@example.com",
		ACMEDirectory:     "https://acme.example/directory",
	})

	for _, want := range []string{
		"--domain dokkup.example.com",
		"--manage-certificate",
		"--acme-email operator@example.com",
		"--acme-directory https://acme.example/directory",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("the unit is missing %q:\n%s", want, unit)
		}
	}
}

// A domain is not consent to replace the certificate at it. An operator who
// supplied one -- an internal CA, a wildcard bought for the year -- has a domain
// too, and renewing over the top of it would replace what they chose with
// something they did not ask for.
func TestADomainAloneDoesNotMakeTheServiceGetItsOwnCertificate(t *testing.T) {
	t.Parallel()

	unit := service.UnitFile(service.UnitConfig{Domain: "dokkup.example.com"})

	if !strings.Contains(unit, "--domain dokkup.example.com") {
		t.Errorf("the unit does not carry the domain:\n%s", unit)
	}
	if strings.Contains(unit, "--manage-certificate") {
		t.Errorf("the unit renews a certificate the operator supplied themselves:\n%s", unit)
	}
}

// The round trip is the point: update finds the health endpoint by reading back
// what the unit says, so a change to either side that broke the pairing would
// leave update polling an address nothing is listening on.
func TestTheAddressCanBeReadBackOutOfTheUnitItWasWrittenInto(t *testing.T) {
	t.Parallel()

	for name, want := range map[string]string{
		"the default":     service.DefaultListen,
		"another port":    "127.0.0.1:9999",
		"every interface": "0.0.0.0:8080",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			unit := service.UnitFile(service.UnitConfig{Listen: want})

			got, ok := service.ListenFromUnit(unit)
			if !ok {
				t.Fatalf("no address found in:\n%s", unit)
			}
			if got != want {
				t.Errorf("listen = %q, want %q", got, want)
			}
		})
	}
}

func TestAHandEditedUnitIsStillRead(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		unit string
		want string
		ok   bool
	}{
		"an equals sign instead of a space": {
			unit: "[Service]\nExecStart=/usr/local/bin/dokkup serve --listen=10.0.0.1:80 --mode ip\n",
			want: "10.0.0.1:80",
			ok:   true,
		},
		"leading whitespace": {
			unit: "[Service]\n  ExecStart=/usr/local/bin/dokkup serve --listen 10.0.0.1:80\n",
			want: "10.0.0.1:80",
			ok:   true,
		},
		// Serve has its own default, so an operator who removed the flag gets
		// the address serve is actually bound to rather than a guess.
		"no address at all": {
			unit: "[Service]\nExecStart=/usr/local/bin/dokkup serve --mode ip\n",
			ok:   false,
		},
		"a trailing --listen with nothing after it": {
			unit: "[Service]\nExecStart=/usr/local/bin/dokkup serve --listen\n",
			ok:   false,
		},
		"no ExecStart": {
			unit: "[Unit]\nDescription=something else\n",
			ok:   false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, ok := service.ListenFromUnit(tc.unit)
			if ok != tc.ok {
				t.Fatalf("found = %v, want %v (got %q)", ok, tc.ok, got)
			}
			if ok && got != tc.want {
				t.Errorf("listen = %q, want %q", got, tc.want)
			}
		})
	}
}
