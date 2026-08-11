// Package scripts_test exercises the shell scripts that CI depends on.
//
// prepare-release.sh decides what version every merge to main becomes, which
// makes it the one piece of release tooling whose mistakes are hard to undo: a
// wrong bump ships under a number that can never be reused, and a missed one
// ships nothing at all and says so quietly. The tests run it against throwaway
// repositories and read back the same $GITHUB_OUTPUT the workflow consumes, so
// what is covered here is the contract the workflow actually relies on rather
// than a restatement of it.
package scripts_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// changelogSkeleton is the smallest file the script accepts: it rebuilds the
// document around the [Unreleased] marker, and reads everything above it back
// unchanged.
const changelogSkeleton = `# Changelog

All notable changes to this project are documented here.

## [Unreleased]
`

func scriptPath(t *testing.T) string {
	t.Helper()

	path, err := filepath.Abs("prepare-release.sh")
	if err != nil {
		t.Fatalf("locating the script: %v", err)
	}
	return path
}

// newRepo returns a repository with the changelog skeleton committed as a chore,
// which is deliberately not releasable on its own.
func newRepo(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "test@example.invalid"},
		{"config", "user.name", "Test"},
	} {
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
	}

	writeChangelog(t, dir, changelogSkeleton)
	commit(t, dir, "chore: seed the repository")
	return dir
}

func writeChangelog(t *testing.T, dir, content string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(dir, "CHANGELOG.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("writing CHANGELOG.md: %v", err)
	}
}

func readChangelog(t *testing.T, dir string) string {
	t.Helper()

	content, err := os.ReadFile(filepath.Join(dir, "CHANGELOG.md"))
	if err != nil {
		t.Fatalf("reading CHANGELOG.md: %v", err)
	}
	return string(content)
}

// commit records message against a change, so that every commit is distinct.
func commit(t *testing.T, dir, message string) {
	t.Helper()

	nonce := filepath.Join(dir, "nonce.txt")
	previous, _ := os.ReadFile(nonce)
	if err := os.WriteFile(nonce, append(previous, byte('x')), 0o644); err != nil {
		t.Fatalf("writing the nonce: %v", err)
	}

	git(t, dir, "add", "-A")

	messageFile := filepath.Join(t.TempDir(), "message")
	if err := os.WriteFile(messageFile, []byte(message), 0o644); err != nil {
		t.Fatalf("writing the commit message: %v", err)
	}
	git(t, dir, "commit", "-q", "-F", messageFile)
}

func tag(t *testing.T, dir, name string) {
	t.Helper()
	git(t, dir, "tag", name)
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

type result struct {
	outputs  map[string]string
	stderr   string
	notes    string
	exitCode int
}

// run invokes the script and reads back the outputs through the same
// $GITHUB_OUTPUT file the workflow reads, rather than parsing its narration.
func run(t *testing.T, dir string, args ...string) result {
	t.Helper()

	outputFile := filepath.Join(t.TempDir(), "github-output")
	notesFile := filepath.Join(t.TempDir(), "release-notes.md")

	cmd := exec.CommandContext(t.Context(), scriptPath(t), append([]string{"--notes-file", notesFile}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GITHUB_OUTPUT="+outputFile,
		// Pinned so that a section heading is a stable string to assert on.
		"RELEASE_DATE=2026-01-02",
	)

	var stderr strings.Builder
	cmd.Stderr = &stderr

	res := result{outputs: map[string]string{}, exitCode: 0}
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("running the script: %v", err)
		}
		res.exitCode = exit.ExitCode()
	}
	res.stderr = stderr.String()

	if raw, err := os.ReadFile(outputFile); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			if key, value, found := strings.Cut(line, "="); found {
				res.outputs[key] = value
			}
		}
	}
	if raw, err := os.ReadFile(notesFile); err == nil {
		res.notes = string(raw)
	}

	return res
}

func (r result) requireRelease(t *testing.T, wantTag string) {
	t.Helper()

	if r.exitCode != 0 {
		t.Fatalf("exit code = %d, want 0: %s", r.exitCode, r.stderr)
	}
	if r.outputs["release"] != "true" {
		t.Fatalf("release = %q, want true: %s", r.outputs["release"], r.stderr)
	}
	if r.outputs["tag"] != wantTag {
		t.Errorf("tag = %q, want %q", r.outputs["tag"], wantTag)
	}
	if want := strings.TrimPrefix(wantTag, "v"); r.outputs["version"] != want {
		t.Errorf("version = %q, want %q: goreleaser and the tag must agree", r.outputs["version"], want)
	}
}

// A cycle of documentation and housekeeping is a real cycle, and it must not
// burn a version number. Exit zero rather than failing: a red X on every docs
// merge teaches people to stop reading red X's.
func TestAMergeWithNothingReleasableProducesNoTagAndStillSucceeds(t *testing.T) {
	t.Parallel()

	dir := newRepo(t)
	tag(t, dir, "v0.4.0")
	before := readChangelog(t, dir)

	for _, message := range []string{
		"docs: explain the trust boundary",
		"chore: bump a development dependency",
		"ci: cache the toolchain",
		"test: cover the parser",
	} {
		commit(t, dir, message)
	}

	res := run(t, dir)

	if res.exitCode != 0 {
		t.Fatalf("exit code = %d, want 0: %s", res.exitCode, res.stderr)
	}
	if res.outputs["release"] != "false" {
		t.Errorf("release = %q, want false", res.outputs["release"])
	}
	if _, tagged := res.outputs["tag"]; tagged {
		t.Error("a tag was emitted for a cycle with nothing releasable")
	}
	if after := readChangelog(t, dir); after != before {
		t.Error("CHANGELOG.md was rewritten even though nothing was released")
	}
}

// The repository has no tags, so the first release has to work from a baseline
// that does not exist yet. git describe cannot do this -- it fails outright.
func TestTheFirstReleaseNeedsNoExistingTag(t *testing.T) {
	t.Parallel()

	dir := newRepo(t)
	commit(t, dir, "feat: scaffold the binary")

	res := run(t, dir)

	res.requireRelease(t, "v0.1.0")
	if res.outputs["previous_tag"] != "" {
		t.Errorf("previous_tag = %q, want empty", res.outputs["previous_tag"])
	}
	if !strings.Contains(readChangelog(t, dir), "[0.1.0]: https://github.com/eduardotorresdev/dokkup/releases/tag/v0.1.0") {
		t.Error("the first version links to its tag, having nothing to compare against")
	}
}

func TestTheStrongestBumpAmongTheCommitsWins(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		from     string
		messages []string
		want     string
	}{
		"a fix alone bumps the patch": {
			from:     "v1.2.3",
			messages: []string{"fix: quote app names"},
			want:     "v1.2.4",
		},
		"a feat outranks a fix regardless of order": {
			from:     "v1.2.3",
			messages: []string{"feat: add scaling", "fix: quote app names"},
			want:     "v1.3.0",
		},
		"a fix merged after a feat does not demote it": {
			from:     "v1.2.3",
			messages: []string{"fix: quote app names", "feat: add scaling"},
			want:     "v1.3.0",
		},
		"a breaking change bumps the major once the major is not zero": {
			from:     "v1.2.3",
			messages: []string{"feat!: drop the old flag"},
			want:     "v2.0.0",
		},
		"a scoped breaking change is still breaking": {
			from:     "v1.2.3",
			messages: []string{"fix(cli)!: rename the flag"},
			want:     "v2.0.0",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			dir := newRepo(t)
			tag(t, dir, tc.from)
			for _, message := range tc.messages {
				commit(t, dir, message)
			}

			run(t, dir).requireRelease(t, tc.want)
		})
	}
}

// 0.x has promised nothing it could break, so a breaking change there is a minor
// bump. Getting this wrong would jump the project to 1.0.0 by accident, and a
// version number cannot be taken back once it is published.
func TestABreakingChangeStaysBelowOneWhileTheMajorIsZero(t *testing.T) {
	t.Parallel()

	dir := newRepo(t)
	tag(t, dir, "v0.3.1")
	commit(t, dir, "feat!: change the install layout")

	res := run(t, dir)

	res.requireRelease(t, "v0.4.0")
	if !strings.Contains(res.stderr, "while the major is 0") {
		t.Errorf("the clamp was not explained on stderr: %q", res.stderr)
	}
}

// The one case where the version rules and the changelog rules disagree.
//
// `chore`, `ci` and `test` are excluded from the notes as housekeeping, but a
// breaking change bumps the version whatever its type. Left alone, a cycle whose
// only releasable commit was `chore!:` would publish a version with an empty
// changelog section and release notes that explain nothing -- which is worse
// than either publishing nothing or publishing a line.
func TestABreakingHousekeepingCommitIsStillWrittenDown(t *testing.T) {
	t.Parallel()

	for _, header := range []string{
		"chore!: drop support for the old data directory",
		"ci!: require a signed tag",
		"test!: remove the compatibility fixtures",
	} {
		t.Run(header, func(t *testing.T) {
			t.Parallel()

			dir := newRepo(t)
			tag(t, dir, "v0.3.1")
			commit(t, dir, header)

			res := run(t, dir)

			res.requireRelease(t, "v0.4.0")

			_, description, _ := strings.Cut(header, ": ")
			if !strings.Contains(res.notes, description) {
				t.Errorf("the release that this commit caused does not mention it:\n%s", res.notes)
			}
		})
	}
}

// Releasing a tag that already exists must publish the prose the file already
// carries.
//
// This is the recovery path in CONTRIBUTING: a tag pushed while goreleaser then
// failed, re-pushed by hand. Regenerating the notes from commit subjects there
// would make the release page disagree with CHANGELOG.md about the same version,
// and would list the bot's own `chore(release):` commit as a change.
func TestTheNotesForATagAlreadyCutComeBackOutOfTheFile(t *testing.T) {
	t.Parallel()

	dir := newRepo(t)
	tag(t, dir, "v0.3.1")
	commit(t, dir, "feat(apps): scale process types")
	commit(t, dir, "fix(dokku): quote app names")

	// Cut it, which writes the section into CHANGELOG.md.
	first := run(t, dir)
	first.requireRelease(t, "v0.4.0")

	// Now ask for that same section back, the way the tag path does.
	again := run(t, dir, "--notes-for", "v0.4.0")

	if again.exitCode != 0 {
		t.Fatalf("exit code = %d, want 0: %s", again.exitCode, again.stderr)
	}
	if again.notes != first.notes {
		t.Errorf("the notes for a re-released tag differ from the ones it was cut with:\n"+
			"cut with:\n%s\nread back:\n%s", first.notes, again.notes)
	}
	if !strings.Contains(again.outputs["notes_arg"], "--release-notes=") {
		t.Errorf("notes_arg = %q, so goreleaser would generate its own notes instead",
			again.outputs["notes_arg"])
	}
	// It must not have tried to work out a version, or it would refuse: v0.4.0
	// exists by now.
	if again.outputs["tag"] != "" {
		t.Errorf("--notes-for computed a version (%q); it exists to read, not to decide",
			again.outputs["tag"])
	}
}

// Publishing empty notes because a section was missing would be worse than
// failing: the release page would silently say nothing about the release.
func TestAskingForNotesThatWereNeverWrittenFails(t *testing.T) {
	t.Parallel()

	dir := newRepo(t)
	commit(t, dir, "feat: something")

	res := run(t, dir, "--notes-for", "v9.9.9")

	if res.exitCode == 0 {
		t.Fatalf("a tag with no changelog section was reported as fine: %s", res.stderr)
	}
	if !strings.Contains(res.stderr, "no section") {
		t.Errorf("stderr does not say what was missing: %q", res.stderr)
	}
}

// The other half: housekeeping that breaks nothing still releases nothing, so
// the rule above cannot be mistaken for "chore now counts".
func TestOrdinaryHousekeepingStillReleasesNothing(t *testing.T) {
	t.Parallel()

	dir := newRepo(t)
	tag(t, dir, "v0.3.1")
	commit(t, dir, "chore: tidy the makefile")
	commit(t, dir, "ci: bump the runner image")

	res := run(t, dir)

	if res.exitCode != 0 {
		t.Fatalf("exit code = %d, want 0: housekeeping is not a failure", res.exitCode)
	}
	if res.outputs["release"] != "false" {
		t.Errorf("release = %q, want false", res.outputs["release"])
	}
}

// The footer is authoritative; prose that merely mentions the phrase is not.
func TestOnlyAFooterAtTheStartOfALineIsABreakingChange(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		message string
		want    string
	}{
		"a real footer": {
			message: "fix: correct the layout\n\nThe body explains it.\n\nBREAKING CHANGE: the flag is gone.\n",
			want:    "v0.4.0",
		},
		"the phrase inside a sentence": {
			message: "fix: correct the layout\n\nWe decided this is not a BREAKING CHANGE: really.\n",
			want:    "v0.3.2",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			dir := newRepo(t)
			tag(t, dir, "v0.3.1")
			commit(t, dir, tc.message)

			run(t, dir).requireRelease(t, tc.want)
		})
	}
}

// Every commit in this repository carries a sign-off, and a trailer is a line of
// the body. Reading any line but the first as a header would classify commits by
// whatever their trailers happen to look like.
func TestOnlyTheFirstLineIsReadAsACommitHeader(t *testing.T) {
	t.Parallel()

	dir := newRepo(t)
	tag(t, dir, "v0.3.1")
	commit(t, dir, "fix: correct the layout\n\nSigned-off-by: Test <test@example.invalid>\n")
	commit(t, dir, "docs: explain it\n\nfeat: this line is prose, not a header\n\nSigned-off-by: Test <test@example.invalid>\n")

	res := run(t, dir)

	res.requireRelease(t, "v0.3.2")
	if strings.Contains(res.notes, "this line is prose") {
		t.Error("a body line was promoted into the changelog as though it were a commit header")
	}
}

// A release candidate is not a release. Treating one as the baseline would make
// the next stable version derive from it and skip the commits before it.
func TestAPrereleaseTagIsNotTakenAsTheBaseline(t *testing.T) {
	t.Parallel()

	dir := newRepo(t)
	tag(t, dir, "v0.5.0")
	commit(t, dir, "feat: something new")
	tag(t, dir, "v0.6.0-rc1")
	commit(t, dir, "fix: correct it")

	res := run(t, dir)

	res.requireRelease(t, "v0.6.0")
	if res.outputs["previous_tag"] != "v0.5.0" {
		t.Errorf("previous_tag = %q, want v0.5.0", res.outputs["previous_tag"])
	}
	if !strings.Contains(res.notes, "something new") {
		t.Error("the commits before the release candidate were dropped from the notes")
	}
}

// Some releases need prose a commit subject cannot carry. Whatever is written by
// hand wins, and is emptied afterwards so it cannot ship twice.
func TestAHandWrittenUnreleasedSectionIsPromotedVerbatimAndThenCleared(t *testing.T) {
	t.Parallel()

	dir := newRepo(t)
	tag(t, dir, "v0.3.1")
	writeChangelog(t, dir, changelogSkeleton+`
### Added

- A paragraph a commit subject could never carry, written by a person.
`)
	commit(t, dir, "docs: write the release notes by hand")
	commit(t, dir, "feat: the thing those notes describe")

	res := run(t, dir)
	res.requireRelease(t, "v0.4.0")

	if !strings.Contains(res.notes, "written by a person") {
		t.Errorf("the hand-written section was not used as the notes: %q", res.notes)
	}
	if strings.Contains(res.notes, "the thing those notes describe") {
		t.Error("generated entries were mixed into a hand-written section")
	}

	after := readChangelog(t, dir)
	unreleased, _, found := strings.Cut(after, "## [0.4.0]")
	if !found {
		t.Fatalf("no released section in the rewritten file:\n%s", after)
	}
	if _, body, _ := strings.Cut(unreleased, "## [Unreleased]"); strings.TrimSpace(body) != "" {
		t.Errorf("[Unreleased] still holds prose that has already shipped: %q", body)
	}
}

func TestGeneratedEntriesAreGroupedAndScopesArePreserved(t *testing.T) {
	t.Parallel()

	dir := newRepo(t)
	tag(t, dir, "v0.3.1")
	commit(t, dir, "feat(api): list apps")
	commit(t, dir, "fix(dokku): quote app names")
	commit(t, dir, "docs: state the threat model")
	commit(t, dir, "refactor: tidy the seam")
	commit(t, dir, "ci: cache the toolchain")

	res := run(t, dir)
	res.requireRelease(t, "v0.4.0")

	for _, want := range []string{
		"### Added\n\n- **api:** list apps",
		"### Changed\n\n- tidy the seam",
		"### Fixed\n\n- **dokku:** quote app names",
		"### Documentation\n\n- state the threat model",
	} {
		if !strings.Contains(res.notes, want) {
			t.Errorf("notes are missing %q:\n%s", want, res.notes)
		}
	}

	// Kept out of the changelog exactly as .goreleaser.yaml excludes it, and it
	// is what keeps the chore(release) commit from feeding the pipeline itself.
	if strings.Contains(res.notes, "cache the toolchain") {
		t.Error("a ci: commit reached the changelog")
	}
	if strings.Contains(res.notes, "## [0.4.0]") {
		t.Error("the notes carry the version heading; the release page supplies its own title")
	}
}

// The baseline only looks at tags reachable from HEAD, so a tag cut on another
// branch is invisible to it and the computed version can collide with one.
func TestATagCutOnAnotherBranchStopsTheRunRatherThanCollidingWithIt(t *testing.T) {
	t.Parallel()

	dir := newRepo(t)
	tag(t, dir, "v0.3.1")

	git(t, dir, "checkout", "-q", "-b", "side", "v0.3.1")
	commit(t, dir, "feat: an attempt that was cut elsewhere")
	tag(t, dir, "v0.4.0")
	git(t, dir, "checkout", "-q", "main")

	commit(t, dir, "feat: something new")
	before := readChangelog(t, dir)

	res := run(t, dir)

	if res.exitCode == 0 {
		t.Fatalf("exit code = 0, want non-zero: %s", res.stderr)
	}
	if !strings.Contains(res.stderr, "v0.4.0 already exists") {
		t.Errorf("stderr does not name the tag: %q", res.stderr)
	}
	if after := readChangelog(t, dir); after != before {
		t.Error("CHANGELOG.md was rewritten by a run that refused to release")
	}
}

func TestAChangelogWithoutTheMarkerIsRefusedRatherThanRebuilt(t *testing.T) {
	t.Parallel()

	dir := newRepo(t)
	writeChangelog(t, dir, "# Changelog\n\nNothing here yet.\n")
	commit(t, dir, "docs: replace the changelog")
	commit(t, dir, "feat: something new")
	before := readChangelog(t, dir)

	res := run(t, dir)

	if res.exitCode == 0 {
		t.Fatal("exit code = 0, want non-zero for a file the script cannot safely rebuild")
	}
	if after := readChangelog(t, dir); after != before {
		t.Error("CHANGELOG.md was modified by a run that refused to proceed")
	}
}

func TestASecondReleaseKeepsTheEarlierSectionAndOnlyOneUnreleasedLink(t *testing.T) {
	t.Parallel()

	dir := newRepo(t)
	commit(t, dir, "feat: the first thing")
	run(t, dir).requireRelease(t, "v0.1.0")

	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "chore(release): v0.1.0")
	tag(t, dir, "v0.1.0")
	commit(t, dir, "feat: the second thing")

	run(t, dir).requireRelease(t, "v0.2.0")

	after := readChangelog(t, dir)
	if strings.Count(after, "\n[unreleased]: ") != 1 {
		t.Errorf("expected exactly one [unreleased] definition:\n%s", after)
	}
	for _, want := range []string{"## [0.2.0]", "## [0.1.0]", "the first thing", "the second thing"} {
		if !strings.Contains(after, want) {
			t.Errorf("the rewritten file lost %q:\n%s", want, after)
		}
	}
	if strings.Index(after, "## [0.2.0]") > strings.Index(after, "## [0.1.0]") {
		t.Error("versions are not newest first")
	}
}

// The release job runs this by hand before it runs it for real, and a preview
// that writes is not a preview.
func TestDryRunChangesNothing(t *testing.T) {
	t.Parallel()

	dir := newRepo(t)
	tag(t, dir, "v0.3.1")
	commit(t, dir, "feat: something new")
	before := readChangelog(t, dir)

	res := run(t, dir, "--dry-run")

	if res.exitCode != 0 {
		t.Fatalf("exit code = %d, want 0: %s", res.exitCode, res.stderr)
	}
	if after := readChangelog(t, dir); after != before {
		t.Error("--dry-run rewrote CHANGELOG.md")
	}
	if _, emitted := res.outputs["release"]; emitted {
		t.Error("--dry-run wrote to $GITHUB_OUTPUT, which would let a preview drive a release")
	}
	if !strings.Contains(res.stderr, "would release v0.4.0") {
		t.Errorf("--dry-run did not report what it would do: %q", res.stderr)
	}
}
