#!/usr/bin/env bash
#
# Woolwire local build.
#
# Where things end up, and why there are three places:
#
#   web/dist   The React bundle. Committed on purpose: web/embed.go embeds it
#              into the Go binary, so a stale one ships a stale UI. Rebuilt by
#              the `web` step, which tells you when it changed.
#   bin/       Host-native binaries you run directly on this machine, for the
#              edit-build-run loop. Gitignored, regenerate freely.
#   dist/      Anything meant to leave this machine: cross-compiled binaries
#              and their checksums (`release`), and the bundle handed to a
#              tester (`share`). Gitignored. The layout matches what the
#              release workflow publishes, so a local build and a tagged
#              release produce the same file names.
#
# Container images are not written to any of those. They are built by the
# compose profile itself from the Dockerfiles, which compile their own copies
# of the web bundle and the binaries inside the image.
#
# Usage:
#   scripts/build.sh                 web + binaries + images + restart the stack
#   scripts/build.sh binaries        just the host-native binaries in bin/
#   scripts/build.sh web             just the embedded UI bundle
#   scripts/build.sh stack           rebuild the images and restart the stack
#   scripts/build.sh release         cross-compile every platform into dist/
#   scripts/build.sh share           assemble the tester bundle in dist/share/
#   scripts/build.sh clean           remove bin/, dist/, and stale binaries
#
# Options:
#   --profile NAME   compose profile under deploy/ (default: managed)
#   --no-web         skip rebuilding the UI bundle
#   --no-cache       build container images from scratch
#   --prune          delete dangling images left behind by earlier builds
#   -h, --help       this text
#
# Environment:
#   WOOLWIRE_PROFILE   same as --profile

set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "$repo_root"

profile=${WOOLWIRE_PROFILE:-managed}
skip_web=0
no_cache=0
prune=0
targets=()

# Every platform the release workflow publishes. Keep these in step with
# .github/workflows/release.yml so a local `release` build is comparable.
release_platforms=(linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64)
# The tester bundle carries only the app, and only for machines a tester is
# likely to have. Its README says to pick the one file that matches.
share_platforms=(linux/amd64 darwin/amd64 darwin/arm64 windows/amd64)

say() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
note() { printf '    %s\n' "$*"; }
die() { printf '\033[31merror: %s\033[0m\n' "$*" >&2; exit 1; }

usage() {
	# The header comment is the help text; print it up to the first line of code.
	awk 'NR>2 { if ($0 !~ /^#/) exit; sub(/^# ?/, ""); print }' "${BASH_SOURCE[0]}"
}

while [ $# -gt 0 ]; do
	case "$1" in
	--profile) [ $# -ge 2 ] || die "--profile needs a name"; profile=$2; shift 2 ;;
	--profile=*) profile=${1#*=}; shift ;;
	--no-web) skip_web=1; shift ;;
	--no-cache) no_cache=1; shift ;;
	--prune) prune=1; shift ;;
	-h | --help) usage; exit 0 ;;
	-*) die "unknown option $1 (try --help)" ;;
	*) targets+=("$1"); shift ;;
	esac
done

[ ${#targets[@]} -gt 0 ] || targets=(dev)

# out_name maps a GOOS/GOARCH pair to the file name the release workflow uses.
out_name() {
	local cmd=$1 goos=$2 goarch=$3 ext=""
	[ "$goos" = windows ] && ext=.exe
	printf '%s-%s-%s%s' "$cmd" "$goos" "$goarch" "$ext"
}

build_go() { # build_go <cmd> <output path> [GOOS] [GOARCH]
	local cmd=$1 out=$2 goos=${3:-} goarch=${4:-}
	# -trimpath everywhere so the binary does not carry this machine's paths;
	# -s -w only for what ships, matching the release workflow.
	if [ -n "$goos" ]; then
		CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch \
			go build -trimpath -ldflags="-s -w" -o "$out" "./cmd/$cmd"
	else
		CGO_ENABLED=0 go build -trimpath -o "$out" "./cmd/$cmd"
	fi
}

target_web() {
	say "Building the embedded UI bundle"
	[ -d web/node_modules ] || npm --prefix web ci
	npm --prefix web run build
	# web/dist is committed, so a changed bundle is something to commit, not
	# just a build artifact. CI fails on a stale one.
	if [ -n "$(git status --porcelain -- web/dist)" ]; then
		note "web/dist changed — commit it with the frontend change."
	fi
}

target_binaries() {
	say "Building host-native binaries into bin/"
	mkdir -p bin
	build_go woolwire bin/woolwire
	build_go woolwire-runner bin/woolwire-runner
	note "bin/woolwire         run with: ./bin/woolwire -state ./state"
	note "bin/woolwire-runner"
}

compose_dir() {
	local dir="deploy/$profile"
	[ -f "$dir/compose.yaml" ] || die "no compose profile at $dir (expected deploy/<name>/compose.yaml)"
	printf '%s' "$dir"
}

# compose runs the profile from its own directory. That is not cosmetic: the
# compose project name comes from the directory, and it is what ties the
# running containers to their named volumes. Invoking the same file from
# elsewhere creates a second project and leaves the existing state volume
# behind, which looks exactly like losing the database.
compose() {
	local dir
	dir=$(compose_dir)
	(cd "$dir" && "${compose_cmd[@]}" "$@")
}

resolve_compose() {
	if command -v podman-compose >/dev/null 2>&1; then
		compose_cmd=(podman-compose)
	elif podman compose version >/dev/null 2>&1; then
		compose_cmd=(podman compose)
	elif docker compose version >/dev/null 2>&1; then
		compose_cmd=(docker compose)
	else
		die "no compose provider found (install podman-compose)"
	fi
}

# Every profile pins the same container_name, so only one of them can run at a
# time. Saying so here beats a bare "name is already in use" from the engine
# halfway through a rebuild, with the other profile's stack already torn down.
assert_no_foreign_stack() {
	local engine=podman name owner
	[ "${compose_cmd[0]}" = "docker" ] && engine=docker
	for name in woolwire-app woolwire-runner; do
		owner=$("$engine" inspect "$name" \
			--format '{{index .Config.Labels "com.docker.compose.project"}}' 2>/dev/null) || continue
		if [ -n "$owner" ] && [ "$owner" != "$profile" ]; then
			die "$name is already running under the '$owner' profile. Stop it first: (cd deploy/$owner && ${compose_cmd[*]} down)"
		fi
	done
}

target_stack() {
	# Validate the profile before announcing anything, so a typo fails on its
	# own line instead of under a heading claiming work started.
	compose_dir >/dev/null
	resolve_compose
	assert_no_foreign_stack
	say "Building images and restarting the '$profile' stack with ${compose_cmd[*]}"

	local build_args=(build)
	[ "$no_cache" -eq 1 ] && build_args+=(--no-cache)
	compose "${build_args[@]}"

	# down before up, rather than `up -d --build` alone: compose recreates a
	# container when its own configuration changes, and a rebuilt image under
	# an unchanged configuration is not that. Without the down, the stack keeps
	# serving the previous image and the rebuild appears to have done nothing.
	# Named volumes survive a down; only `down -v` would remove them.
	compose down
	compose up -d

	compose ps
}

target_release() {
	say "Cross-compiling release binaries into dist/"
	# Clear only what this target owns. dist/share is a separate bundle and a
	# blanket rm here would quietly delete it.
	rm -f dist/woolwire-* dist/SHA256SUMS*.txt
	mkdir -p dist
	local platform goos goarch cmd
	for platform in "${release_platforms[@]}"; do
		goos=${platform%/*}
		goarch=${platform#*/}
		for cmd in woolwire woolwire-runner; do
			note "$(out_name "$cmd" "$goos" "$goarch")"
			build_go "$cmd" "dist/$(out_name "$cmd" "$goos" "$goarch")" "$goos" "$goarch"
		done
	done
	(cd dist && sha256sum woolwire-* > SHA256SUMS.txt)
	note "checksums in dist/SHA256SUMS.txt"
}

target_share() {
	say "Assembling the tester bundle in dist/share/"
	[ -f deploy/share/READ-ME-FIRST.txt ] || die "missing deploy/share/READ-ME-FIRST.txt"
	rm -rf dist/share
	mkdir -p dist/share
	local platform goos goarch
	for platform in "${share_platforms[@]}"; do
		goos=${platform%/*}
		goarch=${platform#*/}
		note "$(out_name woolwire "$goos" "$goarch")"
		build_go woolwire "dist/share/$(out_name woolwire "$goos" "$goarch")" "$goos" "$goarch"
	done
	cp deploy/share/READ-ME-FIRST.txt dist/share/
	# The README tells the tester to verify with `sha256sum -c SHA256SUMS.txt`,
	# so the sums must cover the binaries and not themselves.
	(cd dist/share && sha256sum woolwire-* > SHA256SUMS.txt)
	note "hand over the whole dist/share directory"
}

target_clean() {
	say "Removing build output"
	rm -rf bin dist
	# An earlier build wrote the app to the repository root. It is gitignored
	# and nothing refers to it, so it is only ever a stale copy to run by
	# mistake.
	rm -f woolwire
	note "bin/ dist/ ./woolwire"
	note "web/dist is left alone: it is committed and embedded in the binary."
}

target_prune() {
	resolve_compose
	say "Removing dangling images"
	if [ "${compose_cmd[0]}" = "docker" ]; then
		docker image prune -f
	else
		podman image prune -f
	fi
}

for target in "${targets[@]}"; do
	case "$target" in
	dev)
		[ "$skip_web" -eq 1 ] || target_web
		target_binaries
		target_stack
		;;
	web) target_web ;;
	binaries) [ "$skip_web" -eq 1 ] || target_web; target_binaries ;;
	images | stack) target_stack ;;
	release) [ "$skip_web" -eq 1 ] || target_web; target_release ;;
	share) [ "$skip_web" -eq 1 ] || target_web; target_share ;;
	clean) target_clean ;;
	prune) target_prune ;;
	*) die "unknown target '$target' (try --help)" ;;
	esac
done

if [ "$prune" -eq 1 ]; then
	target_prune
fi

say "Done"
