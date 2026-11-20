#!/usr/bin/env bash
# setup.sh: the bootstrap of the console (docs/design/phase-7.md section 3).
# It answers help and a usage error itself, then checks bash, the root rule
# and Docker with Compose and its buildx component, exports
# bin/stargaite-console when it is missing or of another version than this
# clone, and runs it with the arguments: every subcommand is the console's.
#
# The whole body is in functions and the last line runs main, then exit: bash
# reads a script as it runs, so a change to this file while it runs cannot
# change the lines still to run.
set -u

say() { printf '%s\n' "$*"; }

die() {
	printf 'setup.sh: %s\n' "$*" >&2
	exit 1
}

usage_error() {
	printf 'setup.sh: %s\n' "$*" >&2
	exit 2
}

# at_least VERSION MINIMUM: true when the dotted VERSION, with or without a
# leading v, is MINIMUM or later.
at_least() {
	local -a a b
	local i x
	IFS=. read -ra a <<<"${1#v}"
	IFS=. read -ra b <<<"$2"
	for i in 0 1 2; do
		x=${a[i]-0}
		x=${x%%[!0-9]*}
		((${x:-0} != b[i])) && return $((${x:-0} < b[i]))
	done
	return 0
}

# root_rule: root in a clone that belongs to another user is refused before
# anything is written, git objects included.
root_rule() {
	if [[ $(id -u) == 0 && $(stat -c %u .) != 0 ]]; then
		die "run as root in a clone that belongs to another user, setup.sh would leave root-owned files (.env, tls/, backups/, git objects) that break its next run and that user's git: run it as the clone's owner"
	fi
}

# check_docker: Docker, its daemon, Compose and a .env Compose reads, or what
# is missing appended to the caller's missing.
check_docker() {
	local v
	if ! command -v docker >/dev/null; then
		missing+=("Docker: see https://docs.docker.com/engine/install/")
	elif ! v=$(docker info --format '{{.ServerVersion}}' 2>&1); then
		if [[ $v == *"permission denied"* ]]; then
			missing+=("permission to use Docker: run as root, or join the docker group, which is equivalent to root: sudo usermod -aG docker \"\$USER\", then log in again")
		else
			missing+=("a reachable Docker daemon (docker info: ${v%%$'\n'*}): sudo systemctl start docker")
		fi
	elif ! v=$(docker compose version --short 2>/dev/null) || ! at_least "$v" 2.24.4; then
		missing+=("Docker Compose 2.24.4 or later${v:+, found $v}: see https://docs.docker.com/compose/install/linux/")
	elif ! v=$(docker compose ps -q 2>&1 >/dev/null); then
		# Compose quotes the rest of a .env line it cannot parse, which may be
		# a credential: its words after the line number are left out.
		v=${v%%$'\n'*}
		[[ $v =~ ^(.*line\ [0-9]+) ]] && v=${BASH_REMATCH[1]}
		missing+=("a .env and compose.override.yaml that Compose reads (docker compose ps: ${v%%$'\n'*})")
	fi
}

# report_missing: nothing missing, or all of it with where to get it (nothing
# is ever installed here), then exit 1.
report_missing() {
	((${#missing[@]} == 0)) && return 0
	say "setup.sh needs, before anything else:"
	printf '  - %s\n' "${missing[@]}"
	exit 1
}

# clone_version: STARGAITE_VERSION, the clone's git describe as make image
# takes it, dev without git; exported for the build.
clone_version() {
	command -v git >/dev/null && STARGAITE_VERSION=$(git describe --tags --always --dirty 2>/dev/null) || STARGAITE_VERSION=dev
	export STARGAITE_VERSION
}

# console ARGS...: this script becomes the console, bin/stargaite-console
# (docs/design/phase-7.md section 3), run with ARGS. It is exported first from
# the Dockerfile's console stage when it is missing, reports another version
# than the clone's, or either is dev or dirty. The export needs Docker's
# buildx component; it is bounded to 20 minutes, and killed 10 s later if it
# outlives the stop; --foreground keeps it in the terminal's process group,
# so that Ctrl-C reaches it.
console() {
	local -a missing=()
	local have=""
	root_rule
	check_docker
	((${#missing[@]})) || docker buildx version >/dev/null 2>&1 ||
		missing+=("Docker's buildx component, which builds the console: docker-buildx-plugin from Docker's packages, or docker-buildx on Debian")
	report_missing
	clone_version
	[[ -x bin/stargaite-console ]] && have=$(timeout 10 bin/stargaite-console --version 2>/dev/null)
	if [[ $have != "$STARGAITE_VERSION" || $have == dev || $have == *-dirty ]]; then
		say "building the console, version $STARGAITE_VERSION, into bin/"
		# --quiet: only a failure prints, on standard error.
		VERSION=$STARGAITE_VERSION DOCKER_BUILDKIT=1 timeout --foreground -k 10 1200 \
			docker build --quiet --build-arg VERSION --target console --output type=local,dest=bin . >/dev/null ||
			die "the console was not built within 20 minutes or failed; Docker's output is above"
	fi
	exec bin/stargaite-console "$@"
}

usage() {
	printf '%s\n' "usage: ./setup.sh [subcommand] [flags]

The bootstrap of the console: with no subcommand at a terminal, its home;
a subcommand runs its task, guided at a terminal and by its flags without
one, where a missing answer exits 2 naming its flag.

subcommands:
  install   the first run: this machine, then a provider, an alias with its
            fallbacks and a key, each kept when it exists, then a first
            answer; the default at a terminal before anything is set up
  provider  add providers and their keys; each key is checked for free before it is written
  alias     an alias toward a provider's model, with fallbacks from any provider (--fallback a/m,b/m);
            its new targets are checked first, each paid call counted and announced
  key       a client key for one or more aliases (--alias a,b; main), shown once, then a first answer
  budget    a spending limit on the instance, an alias or a key; the prices
            of the targets it reaches unpriced are asked first
  telemetry traces to Langfuse or any OTLP endpoint, replacing what was set
            whole, then telemetry test
  remote    publish the data plane beyond loopback, always with TLS: your
            certificate or a self-signed one; run again, it renews it
  status    what exists: providers, aliases, keys, budgets, telemetry and both
            versions; with --check, first whether the clone's upstream has
            an upgrade
  logs      the container's last log lines; at a terminal it then follows them until Esc
  stop, start, restart
            the container; stop and restart need --yes without a terminal
  backup    a copy of the store into backups/
  restore   put a copy back (--from <file>, --yes): keys revoked since come back,
            spend since is forgotten; the container is stopped and started again
  uninstall the container, its images and the console; the volume, .env, the
            override, backups/ and tls/ are kept (--yes)
  upgrade   fast-forward this clone to its upstream and rebuild, keeping the
            previous image and taking a backup just before the new version
            starts, then checking it; it prints the way back
  help      this text

install flags, which provider, alias and key take for their steps:
  --port <n>                the host port of the data plane on 127.0.0.1 (4000)
  --provider <name>         a preset, a provider already added, or a new name
  --base-url <url>          the provider's base URL, when its preset has none
  --protocol <p>            a new provider's protocol: openai or anthropic (openai)
  --api-key-env <VAR>       a new provider's credential variable, or none
  --key-env <VAR>           the variable of this script's environment that
                            holds the credential; typed hidden otherwise
  --replace                 replace the credential already in .env
  --allow-private-network   allow a plain-HTTP or private provider URL
  provider also takes several: --provider a,b with --key-env a=VAR,b=VAR;
  then --base-url, --protocol, --api-key-env and --allow-private-network
  are refused
  --model <id>              the model id at the provider
  --reverify                verify the target again although it is verified
  --alias <name>            the alias clients call (main)
  --max-output-tokens <n>   the alias's output cap, for an anthropic target (8192)
  --effort <level>          the alias's default reasoning effort (none)
  --fallback <p/m,…>        the alias's fallbacks, in order, of providers added
  --input-price <usd>, --output-price <usd>
                            per million tokens, when a budget reaches the new
                            target unpriced
  --key-name <name>         the client key's name (my-app)
  --rpm-limit <n>           the client key's requests a minute (60)
  --yes                     confirm a change that replaces something or
                            briefly interrupts traffic, without a terminal

budget flags, with --yes to replace a budget:
  --on <scope>              instance, alias:<name> or key:<name>
  --limit-usd <usd>         the limit
  --reset <interval>        daily, monthly or never (monthly)
  --input-price <usd>, --output-price <usd>
                            per million tokens, for each target it reaches
                            unpriced; as of today
  --reverify                check again the targets whose verification a
                            price cleared, paid; otherwise only said

telemetry flags, with --key-env, --replace, --allow-private-network and --yes:
  --backend <b>             langfuse, otlp, or off to turn traces off
  --endpoint <url>          the OTLP endpoint in full; for Langfuse,
                            https://<host>/api/public/otel
  --langfuse-public-env <VAR>, --langfuse-secret-env <VAR>
                            the variables of this script's environment that
                            hold Langfuse's keys; typed hidden otherwise
  --header-name <name>      an OTLP header that carries a credential (none)

remote flags, with --yes:
  --address <a,b>           IPv4 addresses of this host to publish on beside
                            127.0.0.1 (those published; none the first time)
  --tls <choice>            own, your certificate, or self-signed, made here
  --tls-cert <file>, --tls-key <file>
                            your certificate chain and its key, in PEM, as
                            absolute paths
  --tls-name <name>         the name on it that the admin CLI reaches it by
  --tls-ca <file>           the private CA that signed it, as an absolute
                            path (none: a public CA)

upgrade flags, with --yes:
  --rebuild                 rebuild this clone's version, without fetching
  --legacy-backup <file>    a copy made by hand, when the running version
                            predates backup -; the console says how to make it

For what has no flag here (reorder an alias, see what it sends, change a
provider or a key, see usage and cost, check everything): the guided console,
or the stargaite command: docker compose exec -T stargaite /stargaite help"
}

main() {
	if ((BASH_VERSINFO[0] < 4 || (BASH_VERSINFO[0] == 4 && BASH_VERSINFO[1] < 4))); then
		printf 'setup.sh: needs bash 4.4 or later; this is %s\n' "$BASH_VERSION" >&2
		exit 1
	fi
	trap 'exit 130' INT
	trap 'exit 143' TERM
	# help and a usage error need no Docker and no build (design section 3).
	case ${1-} in
	help | --help | -h)
		usage
		exit 0
		;;
	"") [[ -t 0 && -t 1 ]] || usage_error "no subcommand; ./setup.sh help lists them" ;;
	install | provider | alias | key | budget | telemetry | remote | status | logs | stop | start | restart | backup | restore | uninstall | upgrade) ;;
	*) usage_error "unknown subcommand \"$1\"; ./setup.sh help lists them" ;;
	esac
	if [[ ${BASH_SOURCE[0]} == */* ]]; then
		cd -- "${BASH_SOURCE[0]%/*}" || exit 1
	fi
	[[ -f compose.yaml ]] || die "compose.yaml is not beside setup.sh: run it from the clone of stargaite"
	console "$@"
}

main "$@"; exit
