#!/usr/bin/env bash
#
# Works out the next version from the Conventional Commits merged since the last
# release, and writes the changelog section for it.
#
# It is the single source of truth for both the number and the prose: the
# section written into CHANGELOG.md is the same text handed to goreleaser as the
# release notes, so the file and the release page cannot drift apart.
#
#   scripts/prepare-release.sh --dry-run
#   scripts/prepare-release.sh --notes-file "${RUNNER_TEMP}/release-notes.md"
#
# Bumping follows Conventional Commits with one house rule: while the major
# version is 0, a breaking change bumps the minor rather than the major, because
# 0.x has promised nothing it could break.

set -euo pipefail

REPO_URL="https://github.com/eduardotorresdev/dokkup"
CHANGELOG="CHANGELOG.md"
UNRELEASED_MARKER="## [Unreleased]"

dry_run=false
notes_file=""
notes_for=""

die() {
    printf 'prepare-release: %s\n' "$*" >&2
    exit 1
}

note() {
    printf 'prepare-release: %s\n' "$*" >&2
}

usage() {
    cat >&2 <<'EOF'
usage: scripts/prepare-release.sh [--dry-run] [--notes-file PATH]
       scripts/prepare-release.sh --notes-for vX.Y.Z --notes-file PATH

  --dry-run          Print the plan and the diff; change nothing.
  --notes-file PATH  Also write the changelog section on its own, for
                     goreleaser --release-notes. Must be outside the repository:
                     inside it, it dirties the tree goreleaser is about to build.
  --notes-for TAG    Do not work out a version. Read the section CHANGELOG.md
                     already holds for TAG and write it to --notes-file. This is
                     for releasing a tag that was cut earlier, so that the
                     release page says what the file says.
EOF
}

while [ "$#" -gt 0 ]; do
    case "$1" in
        --dry-run) dry_run=true ;;
        --notes-for)
            shift
            notes_for="${1:-}"
            [ -n "$notes_for" ] || die "--notes-for needs a tag"
            ;;
        --notes-for=*) notes_for="${1#--notes-for=}" ;;
        --notes-file)
            shift
            notes_file="${1:-}"
            [ -n "$notes_file" ] || die "--notes-file needs a path"
            ;;
        --notes-file=*) notes_file="${1#--notes-file=}" ;;
        -h | --help)
            usage
            exit 0
            ;;
        *)
            usage
            die "unknown argument: $1"
            ;;
    esac
    shift
done

root=$(git rev-parse --show-toplevel 2>/dev/null) || die "not inside a git repository"
cd "$root"

[ -f "$CHANGELOG" ] || die "${CHANGELOG} not found"
grep -qxF "$UNRELEASED_MARKER" "$CHANGELOG" \
    || die "${CHANGELOG} has no '${UNRELEASED_MARKER}' line; this script rebuilds the file around it"

# ---------------------------------------------------------------------------
# --notes-for: read back a section that was written by an earlier run
# ---------------------------------------------------------------------------
#
# Releasing a tag that already exists -- the recovery path in CONTRIBUTING, where
# a tag pushed but goreleaser then failed -- must publish the same prose the file
# already carries. Without this the release page would be regenerated from commit
# subjects and disagree with CHANGELOG.md about the very same version, and it
# would list the bot's own `chore(release):` commit.
if [ -n "$notes_for" ]; then
    [ -n "$notes_file" ] || die "--notes-for needs --notes-file to write to"

    # Stops at the next section, and also at the link-reference definitions the
    # file ends with -- the newest section has no section after it, so without
    # that second rule the notes would carry the whole footer.
    want="## [${notes_for#v}]"
    section=$(awk -v want="$want" '
        index($0, want) == 1        { grabbing = 1; next }
        grabbing && /^## \[/        { exit }
        grabbing && /^\[[^]]+\]:[ ]/ { exit }
        grabbing                    { print }
    ' "$CHANGELOG")

    # Trailing and leading blank lines come from the file's own spacing, not from
    # the entry, and goreleaser reproduces the notes byte for byte.
    section=$(printf '%s\n' "$section" | awk '
        { line[NR] = $0 }
        END {
            first = 1; last = NR
            while (first <= last && line[first] ~ /^[[:space:]]*$/) first++
            while (last >= first && line[last] ~ /^[[:space:]]*$/) last--
            for (i = first; i <= last; i++) print line[i]
        }
    ')

    [ -n "$section" ] || die "${CHANGELOG} has no section for ${notes_for}, so there are no notes to publish for it"

    printf '%s\n' "$section" >"$notes_file"
    note "wrote the ${notes_for} section of ${CHANGELOG} to ${notes_file}"
    if [ -n "${GITHUB_OUTPUT:-}" ]; then
        printf 'notes_arg=--release-notes=%s\n' "$notes_file" >>"$GITHUB_OUTPUT"
    fi
    exit 0
fi

# Release dates are UTC. Overridable so that the tests can be deterministic.
release_date="${RELEASE_DATE:-$(date -u +%F)}"

# ---------------------------------------------------------------------------
# Where we are starting from
# ---------------------------------------------------------------------------

# Reachable tags, highest version first, keeping only plain vX.Y.Z. A prerelease
# such as v0.2.0-rc1 is skipped, so the baseline is always the last stable
# release. `git describe` is the wrong tool here: it fails outright when no tag
# exists, which is precisely the case that has to work.
last_tag=$(git tag --list --merged HEAD --sort=-v:refname \
    | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' \
    | head -n 1 || true)

if [ -n "$last_tag" ]; then
    range="${last_tag}..HEAD"
    base="${last_tag#v}"
    note "last release: ${last_tag}"
else
    range="HEAD"
    base="0.0.0"
    note "no release yet; reading the whole history"
fi

major=${base%%.*}
rest=${base#*.}
minor=${rest%%.*}
patch=${rest##*.}

# ---------------------------------------------------------------------------
# One pass over the commits: the bump and the prose come out together
# ---------------------------------------------------------------------------

bump=0 # 0 none, 1 patch, 2 minor, 3 major
added=""
changed=""
fixed=""
documented=""
counted=0

header_re='^([a-zA-Z]+)(\(([^)]*)\))?(!)?:[[:space:]]+(.+)$'

# NUL-separated whole messages, read in this shell rather than a subshell. Two
# details here are load-bearing rather than stylistic:
#
#   -z, not a %x00 in the format. With a format-embedded NUL, git still writes
#   its per-commit newline afterwards, so every record but the first would begin
#   with a blank line and parse as having an empty subject -- which reads as "no
#   releasable commits" and quietly releases nothing.
#
#   A process substitution, not a pipe. A pipe would put this loop in a subshell
#   and discard everything it accumulated.
while IFS= read -r -d '' message; do
    counted=$((counted + 1))

    subject=${message%%$'\n'*}
    body=${message#"$subject"}

    type=""
    scope=""
    breaking=false
    description="$subject"

    # Only the first line is ever read as a header, so a Signed-off-by trailer
    # or a commit list pasted into the body can never be mistaken for one.
    if [[ "$subject" =~ $header_re ]]; then
        type=$(printf '%s' "${BASH_REMATCH[1]}" | tr '[:upper:]' '[:lower:]')
        scope="${BASH_REMATCH[3]}"
        description="${BASH_REMATCH[5]}"
        if [ -n "${BASH_REMATCH[4]}" ]; then
            breaking=true
        fi
    fi

    # Anchored to the start of a line, so prose that merely mentions a breaking
    # change does not become one. A here-string rather than a pipe: under
    # pipefail, `printf | grep -q` can report 141 from SIGPIPE even when it
    # matched, which would lose every footer.
    if grep -Eq '^BREAKING[ -]CHANGE:' <<<"$body"; then
        breaking=true
    fi

    if [ "$breaking" = true ]; then
        level=3
    elif [ "$type" = feat ]; then
        level=2
    elif [ "$type" = fix ]; then
        level=1
    else
        level=0
    fi
    if [ "$level" -gt "$bump" ]; then
        bump="$level"
    fi

    entry="- "
    if [ -n "$scope" ]; then
        entry="${entry}**${scope}:** "
    fi
    entry="${entry}${description}"

    # A commit that moves the version has to appear in the notes, whatever its
    # type says. `chore!:` is housekeeping that broke something, and dropping it
    # would publish a release whose changelog section is empty and whose notes
    # file explains nothing -- the one case where the exclusions below and the
    # bump above disagree.
    if [ "$breaking" = true ]; then
        changed="${changed}${entry}"$'\n'
        continue
    fi

    case "$type" in
        feat) added="${added}${entry}"$'\n' ;;
        fix) fixed="${fixed}${entry}"$'\n' ;;
        docs) documented="${documented}${entry}"$'\n' ;;
        # Housekeeping, excluded exactly as .goreleaser.yaml excludes it. This is
        # also what makes the chore(release) commit inert: it can never bump a
        # version, so the pipeline cannot feed itself.
        chore | ci | test) ;;
        *) changed="${changed}${entry}"$'\n' ;;
    esac
done < <(git log --no-merges --reverse -z --format='%B' "$range" --)

emit() {
    printf '%s\n' "$1" >&2
    if [ -n "${GITHUB_OUTPUT:-}" ]; then
        printf '%s\n' "$1" >>"$GITHUB_OUTPUT"
    fi
}

note "scanned ${counted} commit(s)"

if [ "$bump" -eq 0 ]; then
    note "no feat: or fix: since ${last_tag:-the first commit}; nothing to release"
    emit "release=false"
    exit 0
fi

if [ "$bump" -eq 3 ] && [ "$major" -eq 0 ]; then
    note "breaking change bumps the minor, not the major, while the major is 0"
    bump=2
fi

case "$bump" in
    3)
        major=$((major + 1))
        minor=0
        patch=0
        ;;
    2)
        minor=$((minor + 1))
        patch=0
        ;;
    1) patch=$((patch + 1)) ;;
esac

version="${major}.${minor}.${patch}"
tag="v${version}"

# The baseline only considers tags reachable from HEAD, so the version we just
# computed can still collide with one cut elsewhere -- on a branch, or on a
# commit that has since left this history. Pushing it would fail, or worse
# succeed against the wrong commits, so stop while nothing has been written.
if git rev-parse -q --verify "refs/tags/${tag}" >/dev/null; then
    die "${tag} already exists on a commit outside this history, so it cannot be the next version. Release it from where it was cut, or delete the tag and run again."
fi

# ---------------------------------------------------------------------------
# The section body: hand-written wins, generated is the fallback
# ---------------------------------------------------------------------------

trim_blanks() {
    awk '
        { line[NR] = $0 }
        END {
            first = 1; last = NR
            while (first <= last && line[first] ~ /^[[:space:]]*$/) first++
            while (last >= first && line[last] ~ /^[[:space:]]*$/) last--
            for (i = first; i <= last; i++) print line[i]
        }
    '
}

work=$(mktemp -d)
trap 'rm -rf "${work}"' EXIT INT TERM

# Split the existing file into the parts kept verbatim. The script owns versions,
# never the voice: everything above the marker is read and printed back unchanged.
awk -v marker="$UNRELEASED_MARKER" -v out="$work" '
    $0 == marker             { state = "unreleased"; next }
    state == ""              { print > (out "/head");       next }
    /^\[[^]]*\]:[[:space:]]/ { print > (out "/links");      next }
    /^## \[/                 { state = "rest" }
    state == "unreleased"    { print > (out "/unreleased"); next }
                             { print > (out "/rest") }
' "$CHANGELOG"
: >>"$work/head"
: >>"$work/unreleased"
: >>"$work/rest"
: >>"$work/links"

print_section() {
    [ -n "$2" ] || return 0
    printf '### %s\n\n%s\n\n' "$1" "${2%$'\n'}"
}

promoted=$(trim_blanks <"$work/unreleased")
if [ -n "$promoted" ]; then
    note "promoting the hand-written [Unreleased] section verbatim"
    body="$promoted"
else
    body=$(
        print_section "Added" "$added"
        print_section "Changed" "$changed"
        print_section "Fixed" "$fixed"
        print_section "Documentation" "$documented"
    )
fi

render() {
    trim_blanks <"$work/head"
    printf '\n%s\n\n## [%s] - %s\n\n%s\n' \
        "$UNRELEASED_MARKER" "$version" "$release_date" "$body"

    if [ -s "$work/rest" ]; then
        printf '\n'
        trim_blanks <"$work/rest"
    fi

    printf '\n[unreleased]: %s/compare/%s...HEAD\n' "$REPO_URL" "$tag"
    if [ -n "$last_tag" ]; then
        printf '[%s]: %s/compare/%s...%s\n' "$version" "$REPO_URL" "$last_tag" "$tag"
    else
        printf '[%s]: %s/releases/tag/%s\n' "$version" "$REPO_URL" "$tag"
    fi
    grep -iv '^\[unreleased\]:[[:space:]]' "$work/links" || true
}

render >"$work/CHANGELOG.md"

if [ "$dry_run" = true ]; then
    note "would release ${tag}"
    printf '\n--- %s\n' "$CHANGELOG" >&2
    diff -u "$CHANGELOG" "$work/CHANGELOG.md" >&2 || true
    printf '\n%s\n' "$body"
    exit 0
fi

cp "$work/CHANGELOG.md" "$CHANGELOG"
if [ -n "$notes_file" ]; then
    printf '%s\n' "$body" >"$notes_file"
fi

note "releasing ${tag}"
emit "release=true"
emit "version=${version}"
emit "tag=${tag}"
emit "previous_tag=${last_tag}"
if [ -n "$notes_file" ]; then
    emit "notes_arg=--release-notes=${notes_file}"
else
    emit "notes_arg="
fi
