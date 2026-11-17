#!/usr/bin/env bash
# hoop.sh runs the hoop sidecar for the benchmark (docs/spec-v0.2-benchmark.md 5, docs/lab.md
# "hoop 대비 벤치마크"): it pulls the image and records its digest, then runs it by that digest only,
# with the run's config, the key file and the run's own audit log directory mounted and nothing else.
# It assumes Docker Desktop, where host.docker.internal reaches the host without --add-host.
#
#   hoop.sh pull                         pull $HOOP_IMAGE, check it is the pinned digest, record it
#   hoop.sh start <config.yaml> <name>   check the clock, run the container bench-hoop for the run
#                                        <name>, and wait until it serves
#   hoop.sh validate <config.yaml>       hoop start sidecar --validate on the config, in a throwaway
#                                        container with a throwaway audit directory
#   hoop.sh clock                        compare the containers' clock with the host's
#   hoop.sh stop                         remove the container bench-hoop
#   hoop.sh logs                         its log
#
# Environment: BENCH_DIR (default ~/.local/share/stet/bench-2026-11), DOCKER (default docker),
# HOOP_IMAGE (default hoophq/hoopsidecar:1.310.0), HOOP_DIGEST (the digest pull accepts, default
# 1.310.0's), ANTHROPIC_ENV (default ~/.config/stet/anthropic.env, which sets ANTHROPIC_API_KEY),
# HOOP_USER (the container's user; unset is yours, uid:gid; set and empty is the image's own),
# HOOP_PORT (the port of 127.0.0.1 the container's listener is published on, default 15432),
# HOOP_WAIT (seconds start waits for that port to answer, default 10), HOOP_SETTLE (seconds hoop
# must then stay up, default 2).
set -euo pipefail

BENCH_DIR="${BENCH_DIR:-$HOME/.local/share/stet/bench-2026-11}"
DOCKER="${DOCKER:-docker}"
HOOP_IMAGE="${HOOP_IMAGE:-hoophq/hoopsidecar:1.310.0}"
HOOP_DIGEST="${HOOP_DIGEST:-sha256:737e87b74a682e326a0c39f594a57d3d22e729fc2d106216eef78b47cc7558d1}"
ANTHROPIC_ENV="${ANTHROPIC_ENV:-$HOME/.config/stet/anthropic.env}"
HOOP_USER="${HOOP_USER-$(id -u):$(id -g)}"
HOOP_PORT="${HOOP_PORT:-15432}"
HOOP_WAIT="${HOOP_WAIT:-10}"
HOOP_SETTLE="${HOOP_SETTLE:-2}"

# What the config must say inside the container: start mounts exactly these.
CONFIG_IN=/etc/hoop-inspect/config.yaml # the image's HOOP_SIDECAR_CONFIG
KEY_IN=/run/hoop/anthropic.key
AUDIT_IN=/var/log/hoop
AUDIT_FILE="$AUDIT_IN/audit.jsonl" # what the reuse guard, the runbook and bench collect read
LISTEN=0.0.0.0:15432
PORT=15432
# bench collect joins hoop's audit lines to the writes within 2 s of the sender's clock. The check
# allows 1.5 s for the worst case: the container's reading against either end of the host's window.
MAX_SKEW=1.5
CONTAINER=bench-hoop

usage() {
	echo "usage: hoop.sh pull | start <config.yaml> <name> | validate <config.yaml> | clock | stop | logs" >&2
	echo "(the comment at the top of $0 says what each does)" >&2
	exit 2
}

die() {
	echo "hoop.sh: $*" >&2
	exit 1
}

[[ "$HOOP_PORT" =~ ^[0-9]+$ ]] || die "HOOP_PORT \"$HOOP_PORT\" is not a port"
[[ "$HOOP_WAIT" =~ ^[0-9]+$ ]] || die "HOOP_WAIT \"$HOOP_WAIT\" is not a whole number of seconds"
[[ "$HOOP_SETTLE" =~ ^[0-9]+(\.[0-9]+)?$ ]] || die "HOOP_SETTLE \"$HOOP_SETTLE\" is not a number of seconds"

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

# check_key fails unless the config writes key exactly once, with the value want: an audit log
# written elsewhere would stay in the container and go with it, a key file elsewhere is not there,
# and a listener on another port is not the one published.
check_key() {
	local config="$1" key="$2" want="$3" found
	found="$(sed -nE "s/^[[:space:]]*(- )?$key:[[:space:]]*([^[:space:]#]*).*/\\2/p" "$config")"
	[[ "$found" == "$want" ]] && return
	found="${found//$'\n'/, }"
	die "$config: want $key exactly once, as $want (what start mounts and publishes); found: ${found:-none}"
}

# abs_config prints the config's absolute path, after checking that it fits the mounts.
abs_config() {
	[[ -f "$1" ]] || die "config $1: no such file"
	local config
	config="$(cd "$(dirname "$1")" && pwd)/$(basename "$1")"
	check_key "$config" file "$AUDIT_FILE"
	check_key "$config" credentials_file "$KEY_IN"
	check_key "$config" listen "$LISTEN"
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

# running prints true when the container bench-hoop runs, false when it has stopped, nothing when
# there is none.
running() { "$DOCKER" inspect -f '{{.State.Running}}' "$CONTAINER" 2>/dev/null || true; }

# now prints the host's time in seconds, to the microsecond with perl. Without perl it is whole
# seconds, and the reading that ends a window ($1 = end) is rounded up so the window holds the
# true time.
now() {
	perl -MTime::HiRes=time -e 'printf "%.6f\n", time' 2>/dev/null && return
	if [[ "${1:-}" == end ]]; then echo $(($(date +%s) + 1)); else date +%s; fi
}

utc() {
	perl -MPOSIX=strftime -e 'my $t = shift; printf "%s.%03dZ", strftime("%Y-%m-%dT%H:%M:%S", gmtime int $t), ($t - int $t) * 1000' "$1" 2>/dev/null ||
		printf '%s' "$1"
}

# check_clock reads the container clock between two readings of the host's and fails when it may be
# more than MAX_SKEW seconds off: the bound is the reading's distance from the farther end of the
# host's window, which holds whatever the true offset is. how is run, a new container of the image
# ref (every container shares the clock of Docker's VM, which can drift from the host's), or exec,
# the running bench-hoop, which no container start slows. A reading over the bound is tried again,
# up to three readings, since a slow start widens the window; the smallest bound counts.
check_clock() {
	local ref="$1" how="$2" i t0 c t1 bound best="" label
	label="a new container of the image (docker run)"
	if [[ "$how" == exec ]]; then label="bench-hoop (docker exec)"; fi
	for ((i = 1; i <= 3; i++)); do
		t0="$(now)"
		if [[ "$how" == exec ]]; then
			c="$("$DOCKER" exec "$CONTAINER" date -u +%s.%N)"
		else
			c="$("$DOCKER" run --rm --entrypoint date "$ref" -u +%s.%N)"
		fi
		t1="$(now end)"
		[[ "$c" =~ ^[0-9]+(\.[0-9]+)?$ ]] || die "the container printed no time: \"$c\""
		bound="$(awk -v c="$c" -v a="$t0" -v b="$t1" \
			'BEGIN { x = c - a; if (x < 0) x = -x; y = c - b; if (y < 0) y = -y; printf "%.3f", (x > y ? x : y) }')"
		if [[ -z "$best" ]] || awk -v x="$bound" -v y="${best##* }" 'BEGIN { exit !(x < y) }'; then
			best="$c $t0 $t1 $bound"
		fi
		if awk -v x="$bound" -v m="$MAX_SKEW" 'BEGIN { exit !(x <= m) }'; then break; fi
	done
	set -- $best
	echo "clock, read in $label: container $(utc "$1"), host $(utc "$2") to $(utc "$3"), off by at most $4 s (allowed $MAX_SKEW s)"
	awk -v x="$4" -v m="$MAX_SKEW" 'BEGIN { exit !(x <= m) }' ||
		die "the container clock may be more than $MAX_SKEW s off the host's (bench collect allows 2 s);" \
			"restart Docker (its VM clock drifts after a sleep) and run hoop.sh clock again"
}

# port_answers reports whether something accepts a connection on the published port.
port_answers() { (exec 3<>"/dev/tcp/127.0.0.1/$HOOP_PORT") 2>/dev/null; }

not_serving() {
	echo "hoop.sh: $1; the last lines of its log:" >&2
	"$DOCKER" logs --tail 20 "$CONTAINER" >&2 || true
	die "hoop does not serve; hoop.sh stop removes the container"
}

# wait_serving waits up to HOOP_WAIT seconds for the published port to answer while bench-hoop runs,
# then HOOP_SETTLE seconds more, and asks again whether it runs: Docker's port proxy can answer
# before hoop listens, and hoop exits at once on a config it refuses.
wait_serving() {
	local deadline=$((SECONDS + HOOP_WAIT))
	while :; do
		[[ "$(running)" == true ]] || not_serving "$CONTAINER is not running"
		if port_answers; then break; fi
		((SECONDS < deadline)) || not_serving "nothing answers on 127.0.0.1:$HOOP_PORT after $HOOP_WAIT s"
		sleep 0.2
	done
	sleep "$HOOP_SETTLE"
	[[ "$(running)" == true ]] || not_serving "$CONTAINER is not running $HOOP_SETTLE s after its port answered"
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
	local name="$2" ref config audit used
	bench_dir
	ref="$(digest_ref)"
	[[ "$name" =~ ^[a-z0-9][a-z0-9._-]*$ ]] || die "run name \"$name\": want lowercase letters, digits, ., _ and -"
	config="$(abs_config "$1")"
	# hoop appends to its log: each run gets a directory of its own, and one holding any file with
	# something in it is a used run's. Empty files are what a start that failed leaves.
	audit="$BENCH_DIR/hoop/audit-$name"
	if [[ -d "$audit" ]]; then
		used="$(find "$audit" -type f -size +0c)"
		[[ -z "$used" ]] || die "$audit already holds a run's files (${used//$'\n'/, }); give this run a name of its own"
	fi
	key_file
	mkdir -p "$audit"
	check_clock "$ref" run
	"$DOCKER" run -d --name "$CONTAINER" ${HOOP_USER:+--user "$HOOP_USER"} \
		-p "127.0.0.1:$HOOP_PORT:$PORT" \
		-e HOOP_SIDECAR_FREE_STANDALONE=true -e HOOP_SIDECAR_ANALYTICS=off \
		-v "$config:$CONFIG_IN:ro" \
		-v "$BENCH_DIR/hoop/anthropic.key:$KEY_IN:ro" \
		-v "$audit:$AUDIT_IN" \
		"$ref"
	wait_serving
	echo "hoop.sh: $CONTAINER runs $ref on 127.0.0.1:$HOOP_PORT; config $config; audit log $audit/audit.jsonl"
}

# validate mounts what start mounts, with a throwaway audit directory: a validation never creates or
# touches a run's. It is removed when the script exits.
VALIDATE_DIR=""
cmd_validate() {
	[[ $# -eq 1 ]] || usage
	local ref config
	bench_dir
	ref="$(digest_ref)"
	config="$(abs_config "$1")"
	key_file
	VALIDATE_DIR="$(mktemp -d "$BENCH_DIR/hoop/validate.XXXXXX")"
	trap '[[ -z "$VALIDATE_DIR" ]] || rm -rf "$VALIDATE_DIR"' EXIT
	"$DOCKER" run --rm ${HOOP_USER:+--user "$HOOP_USER"} \
		-e HOOP_SIDECAR_FREE_STANDALONE=true -e HOOP_SIDECAR_ANALYTICS=off \
		-v "$config:$CONFIG_IN:ro" \
		-v "$BENCH_DIR/hoop/anthropic.key:$KEY_IN:ro" \
		-v "$VALIDATE_DIR:$AUDIT_IN" \
		"$ref" hoop start sidecar --validate
}

# clock reads the running bench-hoop's clock when there is one, else a new container's.
cmd_clock() {
	[[ $# -eq 0 ]] || usage
	local ref how=run
	bench_dir
	ref="$(digest_ref)"
	if [[ "$(running)" == true ]]; then how=exec; fi
	check_clock "$ref" "$how"
}

[[ $# -ge 1 ]] || usage
command="$1"
shift
case "$command" in
pull) cmd_pull "$@" ;;
start) cmd_start "$@" ;;
validate) cmd_validate "$@" ;;
clock) cmd_clock "$@" ;;
stop) [[ $# -eq 0 ]] || usage; "$DOCKER" rm -f "$CONTAINER" ;;
logs) [[ $# -eq 0 ]] || usage; "$DOCKER" logs "$CONTAINER" ;;
*) usage ;;
esac
