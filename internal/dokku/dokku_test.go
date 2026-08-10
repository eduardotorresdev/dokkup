package dokku_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/eduardotorresdev/dokkup/internal/dokku"
)

func TestValidateAppNameRejectsNamesThatCouldBeReadAsFlagsOrShellSyntax(t *testing.T) {
	t.Parallel()

	rejected := map[string]string{
		"leading dash reads as a flag":     "-rf",
		"long flag reads as a flag":        "--force",
		"semicolon chains a command":       "app;rm -rf /",
		"backtick substitutes a command":   "app`id`",
		"dollar-paren substitutes":         "app$(id)",
		"pipe redirects":                   "app|sh",
		"space splits into two arguments":  "app other",
		"newline splits into two commands": "app\nrm",
		"slash escapes into a path":        "../etc/passwd",
		"uppercase is not a Dokku name":    "MyApp",
		"empty is not a name":              "",
	}

	for reason, name := range rejected {
		t.Run(reason, func(t *testing.T) {
			t.Parallel()
			if err := dokku.ValidateAppName(name); err == nil {
				t.Fatalf("ValidateAppName(%q) accepted a name it must reject", name)
			}
		})
	}
}

func TestValidateAppNameAcceptsRealDokkuNames(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"api", "web-app", "app2", "0auth", strings.Repeat("a", 63)} {
		if err := dokku.ValidateAppName(name); err != nil {
			t.Errorf("ValidateAppName(%q) rejected a valid name: %v", name, err)
		}
	}
}

func TestValidateAppNameRejectsNamesLongerThanDokkuAllows(t *testing.T) {
	t.Parallel()

	if err := dokku.ValidateAppName(strings.Repeat("a", 64)); err == nil {
		t.Fatal("ValidateAppName accepted a 64-character name")
	}
}

func TestFakeReportsTheAppsItWasGiven(t *testing.T) {
	t.Parallel()

	fake := dokku.NewFake()
	fake.AddApp("web", map[string]string{"app-locked": "false"})
	fake.AddApp("api", nil)

	apps, err := fake.Apps(context.Background())
	if err != nil {
		t.Fatalf("Apps: %v", err)
	}
	if want := []string{"api", "web"}; !slicesEqual(apps, want) {
		t.Fatalf("Apps = %v, want %v", apps, want)
	}
}

func TestFakeReportsAppNotFoundForAnUnknownApp(t *testing.T) {
	t.Parallel()

	fake := dokku.NewFake()

	_, err := fake.AppReport(context.Background(), "missing")
	if !errors.Is(err, dokku.ErrAppNotFound) {
		t.Fatalf("AppReport error = %v, want ErrAppNotFound", err)
	}
}

func TestFakeRejectsAnInvalidAppNameBeforeLookingItUp(t *testing.T) {
	t.Parallel()

	fake := dokku.NewFake()

	if _, err := fake.AppReport(context.Background(), "--force"); err == nil {
		t.Fatal("AppReport accepted an invalid app name")
	}
	// Validation must happen before anything is recorded: the fake stands in for
	// a real invocation, and a rejected name must never reach one.
	if len(fake.Calls) != 0 {
		t.Fatalf("Calls = %v, want none for a rejected name", fake.Calls)
	}
}

func TestFakeReturnsACopySoCallersCannotMutateItsState(t *testing.T) {
	t.Parallel()

	fake := dokku.NewFake()
	fake.AddApp("web", map[string]string{"app-locked": "false"})

	report, err := fake.AppReport(context.Background(), "web")
	if err != nil {
		t.Fatalf("AppReport: %v", err)
	}
	report["app-locked"] = "true"

	again, err := fake.AppReport(context.Background(), "web")
	if err != nil {
		t.Fatalf("AppReport: %v", err)
	}
	if again["app-locked"] != "false" {
		t.Fatalf("app-locked = %q, want %q: the fake handed out its own map", again["app-locked"], "false")
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
