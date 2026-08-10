// Package dokku is the single seam through which dokkup invokes Dokku.
//
// The interface covers only the commands dokkup actually uses, and grows when a
// feature needs one. It is deliberately not a complete binding of the Dokku CLI:
// mirroring a surface we mostly do not use would be a large and permanently
// out-of-date effort. Growing it feature by feature means that when a milestone
// is finished, everything dokkup consumes from Dokku is behind this interface by
// construction, and [Fake] covers exactly the same surface without drifting.
//
// See docs/adr/0003-dokkuclient-is-the-only-seam.md.
package dokku

import (
	"context"
	"errors"
	"fmt"
	"regexp"
)

// Client is how dokkup talks to Dokku. Every implementation must reject invalid
// app names before invoking anything.
//
// Add a method when a feature needs it, and add it to [Fake] in the same change.
type Client interface {
	// Version reports the Dokku version installed on the host.
	Version(ctx context.Context) (string, error)

	// Apps lists the names of every app Dokku knows about.
	Apps(ctx context.Context) ([]string, error)

	// AppReport returns the machine-readable report for one app.
	AppReport(ctx context.Context, app string) (map[string]string, error)
}

// ErrAppNotFound is returned when an operation names an app Dokku does not have.
var ErrAppNotFound = errors.New("dokku: app not found")

// appName matches Dokku's own constraint on app names: lowercase letters,
// digits and dashes, starting with a letter or digit.
//
// This is not cosmetic. Commands are built as argument vectors and executed
// without a shell, so a name cannot be interpreted as shell syntax -- but a name
// beginning with a dash would still be read as a flag by the Dokku binary
// itself. Validating here closes that gap for every caller at once.
var appName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// ValidateAppName reports whether name is usable as a Dokku app name.
func ValidateAppName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("app name is empty")
	case len(name) > 63:
		return fmt.Errorf("app name %q is longer than 63 characters", name)
	case !appName.MatchString(name):
		return fmt.Errorf("app name %q must contain only lowercase letters, digits and dashes, and start with a letter or digit", name)
	default:
		return nil
	}
}
