package dokku_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eduardotorresdev/dokkup/internal/dokku"
)

// stubDokku writes a stand-in for the dokku binary that records the argument
// vector it was given and prints body. It is how the argument vectors and the
// output parsing get tested on a machine with no Dokku on it -- which is where
// they were wrong: `apps:list --quiet` was sent for the whole of this project's
// life, and Dokku 0.38.7 answers `unknown flag: --quiet` and exits 2.
func stubDokku(t *testing.T, body string) (*dokku.ExecClient, func() string) {
	t.Helper()

	dir := t.TempDir()
	binary := filepath.Join(dir, "dokku")
	log := filepath.Join(dir, "args")

	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + log + "'\ncat <<'DOKKU_EOF'\n" + body + "\nDOKKU_EOF\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatalf("writing the stub: %v", err)
	}

	recorded := func() string {
		content, err := os.ReadFile(log)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(content))
	}

	return &dokku.ExecClient{Binary: binary}, recorded
}

// The exact bytes Dokku 0.38.7 prints, header and all.
const appsListOutput = "=====> My Apps\nblog\nprobe\n"

func TestAppsAsksForNothingDokkuDoesNotUnderstand(t *testing.T) {
	t.Parallel()

	client, recorded := stubDokku(t, appsListOutput)

	if _, err := client.Apps(context.Background()); err != nil {
		t.Fatalf("Apps: %v", err)
	}

	// `apps:list` takes no flags. Sending one makes Dokku print usage and exit 2,
	// so the App list would be empty on every host.
	if got := recorded(); got != "apps:list" {
		t.Errorf("dokku was asked %q, want %q", got, "apps:list")
	}
}

func TestAppsReadsTheNamesAndNotDokkusNarration(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		output string
		want   []string
	}{
		"the real output": {output: appsListOutput, want: []string{"blog", "probe"}},
		// Dokku says this instead of printing nothing at all, and it is not an
		// application called "You haven't deployed any applications yet".
		"a host with no apps": {
			output: "=====> My Apps\n !     You haven't deployed any applications yet\n",
			want:   nil,
		},
		"blank lines": {output: "=====> My Apps\n\nblog\n\n", want: []string{"blog"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			client, _ := stubDokku(t, tc.output)

			apps, err := client.Apps(context.Background())
			if err != nil {
				t.Fatalf("Apps: %v", err)
			}
			if !slices.Equal(apps, tc.want) {
				t.Errorf("apps = %v, want %v", apps, tc.want)
			}
		})
	}
}

// A Dokku that has hung must not hold the HTTP request that asked open. The
// timeout alone does not achieve that: it kills dokku and nothing else, while
// Wait goes on waiting for the stdout pipe -- which every process dokku forked,
// and it forks docker, git and plugin hooks, is still holding.
//
// Like its counterpart in internal/service, this only bites on Linux. macOS
// closes the pipes as the process dies and reports the timeout honestly.
func TestAHungDokkuIsGivenUpOn(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	binary := filepath.Join(dir, "dokku")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatalf("writing the stub: %v", err)
	}
	client := &dokku.ExecClient{Binary: binary, Timeout: 100 * time.Millisecond}

	start := time.Now()
	if _, err := client.Version(context.Background()); err == nil {
		t.Fatal("a hung invocation was reported as success")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("waited %v for a 100ms timeout", elapsed)
	}
}

func TestVersionReportsTheNumberWithoutDokkusPreamble(t *testing.T) {
	t.Parallel()

	client, recorded := stubDokku(t, "dokku version 0.38.7")

	version, err := client.Version(context.Background())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if version != "0.38.7" {
		t.Errorf("version = %q, want %q", version, "0.38.7")
	}
	if got := recorded(); got != "version" {
		t.Errorf("dokku was asked %q, want %q", got, "version")
	}
}
