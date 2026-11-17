#!/usr/bin/env bash
# hoop.sh runs the hoop sidecar for the benchmark (docs/spec-v0.2-benchmark.md 5, docs/lab.md
# "hoop 대비 벤치마크"): it pulls the image and records its digest, then runs it by that digest only,
# with the run's config, the key file and the run's own audit log directory mounted and nothing else.
#
#   hoop.sh pull                     pull $HOOP_IMAGE, check it is the pinned digest, record it
#   hoop.sh start <config.yaml> <name>   run the container bench-hoop for the run <name>
#   hoop.sh validate <config.yaml>   hoop start sidecar --validate on the config, in a throwaway container
#   hoop.sh clock                    compare the containers' clock with the host's
#   hoop.sh stop                     remove the container bench-hoop
#   hoop.sh logs                     its log
#
# Environment: BENCH_DIR (default ~/.local/share/stet/bench-2026-11), DOCKER (default docker),
# HOOP_IMAGE (default hoophq/hoopsidecar:1.310.0), HOOP_DIGEST (the digest pull accepts, default
# 1.310.0's), ANTHROPIC_ENV (default ~/.config/stet/anthropic.env, which sets ANTHROPIC_API_KEY),
# HOOP_USER (the container's user; unset is yours, uid:gid; set and empty is the image's own).
set -euo pipefail

BENCH_DIR="${BENCH_DIR:-$HOME/.local/share/stet/bench-2026-11}"
DOCKER="${DOCKER:-docker}"
HOOP_IMAGE="${HOOP_IMAGE:-hoophq/hoopsidecar:1.310.0}"
HOOP_DIGEST="${HOOP_DIGEST:-sha256:737e87b74a682e326a0c39f594a57d3d22e729fc2d106216eef78b47cc7558d1}"
ANTHROPIC_ENV="${ANTHROPIC_ENV:-$HOME/.config/stet/anthropic.env}"
HOOP_USER="${HOOP_USER-$(id -u):$(id -g)}"

# The paths the config must use inside the container: start mounts exactly these.
CONFIG_IN=/etc/hoop-inspect/config.yaml # the image's HOOP_SIDECAR_CONFIG
KEY_IN=/run/hoop/anthropic.key
AUDIT_IN=/var/log/hoop
PORT=15432
# bench collect joins hoop's audit lines to the writes within 2 s of the sender's clock.
MAX_SKEW=1

usage() {
	echo "usage: hoop.sh pull | start <config.yaml> <name> | validate <config.yaml> | clock | stop | logs" >&2
	echo "(the comment at the top of $0 says what each does)" >&2
	exit 2
}

die() {
	echo "hoop.sh: $*" >&2
	exit 1
}

# bench_dir creates BENCH_DIR and makes it absolute: docker -v reads a relative path as a volume name.
bench_dir() {
	mkdir -p "$BENCH_DIR/hoop"
	BENCH_DIR="$(cd "$BENCH_DIR" && pwd)"
}

# digest_ref prints the recorded image reference, which names the image by digest.
digest_ref() {
	local f="$BENCH_DIR/hoop.digest" ref
	[[ -s "$f" ]] || die "no $f: run hoop.sh pull first"
	ref="$(<"$f")"
	[[ "$ref" =~ ^[a-z0-9./_-]+@sha256:[0-9a-f]{64}$ ]] ||
		die "$f holds \"$ref\", not an image named by digest (repo@sha256:<64 hex>): run hoop.sh pull first"
	printf '%s\n' "$ref"
}

# abs_config prints the config's absolute path, after checking that it is a file whose paths fit
# the mounts (an audit log written elsewhere would stay in the container and go with it).
abs_config() {
	[[ -f "$1" ]] || die "config $1: no such file"
	local config
	config="$(cd "$(dirname "$1")" && pwd)/$(basename "$1")"
	grep -Eq "^[[:space:]]*file:[[:space:]]*$AUDIT_IN/[^[:space:]/]+[[:space:]]*\$" "$config" ||
		die "$config: audit.file is not a file in $AUDIT_IN, where the run's audit directory is mounted"
	grep -Eq "^[[:space:]]*credentials_file:[[:space:]]*$KEY_IN[[:space:]]*\$" "$config" ||
		die "$config: analyzer.credentials_file is not $KEY_IN, where the key file is mounted"
	grep -Eq "^[[:space:]]*listen:[[:space:]]*0\\.0\\.0\\.0:$PORT[[:space:]]*\$" "$config" ||
		die "$config: the listener does not listen on 0.0.0.0:$PORT, the port published on 127.0.0.1:$PORT"
	printf '%s\n' "$config"
}

# key_file writes the key alone, mode 0600, from ANTHROPIC_ENV when there is none yet. The key is
# read in a subshell and written by a builtin: it is never printed or passed as an argument.
key_file() {
	local key="$BENCH_DIR/hoop/anthropic.key"
	if [[ ! -s "$key" ]]; then
		[[ -r "$ANTHROPIC_ENV" ]] || die "cannot read $ANTHROPIC_ENV, which sets ANTHROPIC_API_KEY"
		(
			set +x
			umask 077
			# shellcheck source=/dev/null
			. "$ANTHROPIC_ENV"
			[[ -n "${ANTHROPIC_API_KEY:-}" ]] || die "$ANTHROPIC_ENV does not set ANTHROPIC_API_KEY"
			printf '%s' "$ANTHROPIC_API_KEY" >"$key"
		)
	fi
	chmod 600 "$key" # hoop refuses a key file group or others can read
}

hosttime() { perl -MTime::HiRes=time -e 'printf "%.6f\n", time'; }

# check_clock reads the clock of a throwaway container of the image, between two readings of the
# host's, and fails when it lies more than MAX_SKEW seconds outside them. Every container shares
# the clock of Docker's VM, which can drift from the host's (after a sleep, for one).
check_clock() {
	local ref="$1" before container after
	before="$(hosttime)"
	container="$("$DOCKER" run --rm --entrypoint date "$ref" -u +%s.%N)"
	after="$(hosttime)"
	perl -MPOSIX=strftime -e '
		my ($c, $t0, $t1, $max) = @ARGV;
		$c =~ /^\d+(\.\d+)?$/ or die "hoop.sh: the container printed no time: \"$c\"\n";
		sub utc { my $t = shift; sprintf "%s.%03dZ", strftime("%Y-%m-%dT%H:%M:%S", gmtime int $t), ($t - int $t) * 1000 }
		my $off = $c < $t0 ? $c - $t0 : $c > $t1 ? $c - $t1 : 0;
		printf "clock: container %s, host %s to %s, off by %+.3f s (at most %d s)\n", utc($c), utc($t0), utc($t1), $off, $max;
		exit(abs($off) > $max ? 1 : 0);
	' "$container" "$before" "$after" "$MAX_SKEW" ||
		die "the container clock is not within $MAX_SKEW s of the host's (bench collect allows 2 s);" \
			"restart Docker (its VM clock drifts) and run hoop.sh clock again"
}

cmd_pull() {
	[[ $# -eq 0 ]] || usage
	bench_dir
	"$DOCKER" pull "$HOOP_IMAGE"
	local ref
	ref="$("$DOCKER" inspect --format '{{index .RepoDigests 0}}' "$HOOP_IMAGE")"
	[[ "$ref" =~ ^[a-z0-9./_-]+@sha256:[0-9a-f]{64}$ ]] ||
		die "docker inspect names $HOOP_IMAGE \"$ref\", not repo@sha256:<64 hex>; nothing recorded"
	[[ "${ref#*@}" == "$HOOP_DIGEST" ]] ||
		die "$HOOP_IMAGE is $ref, not the pinned $HOOP_DIGEST (spec 5); nothing recorded"
	printf '%s\n' "$ref" >"$BENCH_DIR/hoop.digest"
	echo "$ref"
}

cmd_start() {
	[[ $# -eq 2 ]] || usage
	local name="$2" ref config audit
	bench_dir
	ref="$(digest_ref)"
	[[ "$name" =~ ^[a-z0-9][a-z0-9._-]*$ ]] || die "run name \"$name\": want lowercase letters, digits, ., _ and -"
	config="$(abs_config "$1")"
	# hoop appends to its log: each run gets a directory of its own, and a used one is not taken again.
	audit="$BENCH_DIR/hoop/audit-$name"
	[[ ! -s "$audit/audit.jsonl" ]] ||
		die "$audit/audit.jsonl already holds a run's log; give this run a name of its own"
	key_file
	mkdir -p "$audit"
	check_clock "$ref"
	"$DOCKER" run -d --name bench-hoop ${HOOP_USER:+--user "$HOOP_USER"} \
		-p "127.0.0.1:$PORT:$PORT" \
		-e HOOP_SIDECAR_FREE_STANDALONE=true -e HOOP_SIDECAR_ANALYTICS=off \
		-v "$config:$CONFIG_IN:ro" \
		-v "$BENCH_DIR/hoop/anthropic.key:$KEY_IN:ro" \
		-v "$audit:$AUDIT_IN" \
		"$ref"
	echo "hoop.sh: bench-hoop runs $ref on 127.0.0.1:$PORT; config $config; audit log $audit/audit.jsonl"
}

# validate needs no audit mount: --validate builds the lanes and opens no audit file.
cmd_validate() {
	[[ $# -eq 1 ]] || usage
	local ref config
	bench_dir
	ref="$(digest_ref)"
	config="$(abs_config "$1")"
	key_file
	"$DOCKER" run --rm ${HOOP_USER:+--user "$HOOP_USER"} \
		-e HOOP_SIDECAR_FREE_STANDALONE=true -e HOOP_SIDECAR_ANALYTICS=off \
		-v "$config:$CONFIG_IN:ro" \
		-v "$BENCH_DIR/hoop/anthropic.key:$KEY_IN:ro" \
		"$ref" hoop start sidecar --validate
}

cmd_clock() {
	[[ $# -eq 0 ]] || usage
	local ref
	bench_dir
	ref="$(digest_ref)"
	check_clock "$ref"
}

[[ $# -ge 1 ]] || usage
command="$1"
shift
case "$command" in
pull) cmd_pull "$@" ;;
start) cmd_start "$@" ;;
validate) cmd_validate "$@" ;;
clock) cmd_clock "$@" ;;
stop) [[ $# -eq 0 ]] || usage; "$DOCKER" rm -f bench-hoop ;;
logs) [[ $# -eq 0 ]] || usage; "$DOCKER" logs bench-hoop ;;
*) usage ;;
esac
