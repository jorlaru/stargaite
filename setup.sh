#!/usr/bin/env bash
# setup.sh: the guided assistant (OPS-11). It checks the machine, writes .env
# and compose.override.yaml, starts the container and changes the proxy's
# configuration only through the CLI inside it. It asks only when standard
# input and standard output are both a terminal; every question has a flag.
# ./setup.sh help lists the subcommands and their flags.
#
# The whole body is in functions and the last line runs main, then exit: bash
# reads a script as it runs, so a change to this file while it runs cannot
# change the lines still to run.
# The jq filters are single-quoted on purpose: their $names are jq's.
# shellcheck disable=SC2016
set -u

override_marker='# Written by setup.sh from its "# setup:" lines below; it rewrites this file whole.'
tmp_file="" agreed=0 tls_args=() admin=() published=""
provider="" model="" alias="" key_name="" protocol="" provider_json="" key_value="" client_key=""
declare -A opt=()

say() { printf '%s\n' "$*"; }

die() {
	printf 'setup.sh: %s\n' "$*" >&2
	exit 1
}

usage_error() {
	printf 'setup.sh: %s\n' "$*" >&2
	exit 2
}

interactive() { [[ -t 0 && -t 1 ]]; }

# cleanup runs on every exit: no half-written file stays, and the terminal
# echoes again after a hidden question.
# shellcheck disable=SC2329 # run by the EXIT trap
cleanup() {
	[[ -n $tmp_file ]] && rm -f -- "$tmp_file"
	[[ -t 0 ]] && stty echo 2>/dev/null
	return 0
}

# parse_flags FLAGS BOOLS ARGS...: --name value and --name=value into opt, a
# boolean as 1. Anything outside FLAGS and BOOLS is a wrong usage.
parse_flags() {
	local flags=" $1 " bools=" $2 " a name
	shift 2
	while (($#)); do
		a=$1
		shift
		[[ $a == --?* ]] || usage_error "unexpected argument: $a"
		name=${a#--}
		name=${name%%=*}
		if [[ $bools == *" $name "* && $a != *=* ]]; then
			opt[$name]=1
		elif [[ $flags != *" $name "* ]]; then
			usage_error "unknown flag --$name; ./setup.sh help lists the flags"
		elif [[ $a == *=* ]]; then
			opt[$name]=${a#*=}
		else
			(($#)) || usage_error "--$name needs a value"
			opt[$name]=$1
			shift
		fi
	done
}

# ask VAR FLAG DEFAULT TEXT [optional]: VAR is the value of --FLAG; else, at
# a terminal, the answer to TEXT, Enter taking DEFAULT; else DEFAULT. Without
# a default, a terminal or the flag, exit 2 naming the flag, unless the answer
# is optional, which may stay empty.
ask() {
	local -n _ask=$1
	local answer
	if [[ -v "opt[$2]" ]]; then
		_ask=${opt[$2]}
		return
	fi
	if ! interactive; then
		[[ -n $3 || -n ${5-} ]] || usage_error "missing --$2: $4"
		_ask=$3
		return
	fi
	while :; do
		read -r -p "$4${3:+ [$3]}: " answer || die "no answer to: $4"
		_ask=${answer:-$3}
		[[ -n $_ask || -n ${5-} ]] && return
	done
}

# yesno FLAG TEXT: true with --FLAG, or with y at a terminal; false otherwise.
yesno() {
	local answer
	[[ -v "opt[$1]" ]] && return 0
	interactive || return 1
	read -r -p "$2 [y/N]: " answer && [[ $answer == [yY]* ]]
}

# confirm TEXT: says TEXT, then needs --yes, or y at a terminal; without a
# terminal and --yes, exit 2. Declined, nothing more changes.
confirm() {
	say "$1"
	yesno yes "continue?" && return 0
	interactive || usage_error "missing --yes: $1"
	die "stopped; nothing more was changed"
}

# agree TEXT: says TEXT, a change to .env or the override that the container
# reads only once recreated, and asks for that recreate the first time in a
# run, before the change is made, so a refusal leaves nothing written.
agree() {
	if ((agreed)); then
		say "$1"
		return
	fi
	confirm "$1, and the container is then recreated to read it: requests in flight are given time to finish, and the rate-limit windows restart"
	agreed=1
}

# secret VAR NAME FLAG: the value for the credential variable NAME, from the
# variable of this script's environment that --FLAG names, or typed hidden at
# a terminal. Only builtins touch it. .env holds it single-quoted, so a value
# with ' or a newline, or ending in \, is refused.
secret() {
	local -n _secret=$1
	local from=${opt[$3]-}
	if [[ -n $from ]]; then
		# Not printed: a pasted value instead of a name would reach the screen.
		# Only an exported variable counts, never one of this script's own.
		[[ $from =~ ^[A-Za-z_][A-Za-z0-9_]*$ && $(declare -p -- "$from" 2>/dev/null) =~ ^declare\ -[a-zA-Z]*x ]] ||
			usage_error "--$3 takes the name of a variable exported to this script"
		_secret=${!from}
	elif interactive; then
		read -rs -p "$2 (typed hidden): " _secret || die "no answer to: $2"
		printf '\n'
	else
		usage_error "missing --$3: the variable of this script's environment that holds $2"
	fi
	[[ -n $_secret ]] || die "$2 is empty"
	[[ $_secret != *"'"* && $_secret != *$'\n'* && $_secret != *\\ ]] ||
		die "$2 holds ' or a newline, or ends in \\: .env cannot hold it, so nothing was written"
}

# credentials NAME FLAG...: each credential NAME kept from .env unless
# --replace (or y at a terminal) replaces it, otherwise read by secret from
# FLAG; all are read before any is written, each through agree. key_value is
# the last one's value.
credentials() {
	local -a names=() values=()
	local i
	while (($#)); do
		key_value=""
		if env_get key_value "$1" && ! yesno replace "$1 is in .env: replace it?"; then
			say "$1 is in .env; kept"
		else
			secret key_value "$1" "$2"
			names+=("$1")
			values+=("$key_value")
		fi
		shift 2
	done
	for i in "${!names[@]}"; do
		agree "writing ${names[i]} to .env"
		env_set "${names[i]}" "${values[i]}"
	done
}

# env_get VAR NAME: VAR is the value NAME has in .env, without its quotes;
# false when .env has no NAME line.
env_get() {
	local -n _env=$1
	local line
	[[ -f .env ]] || return 1
	while IFS= read -r line || [[ -n $line ]]; do
		[[ $line == "$2="* ]] || continue
		_env=${line#*=}
		[[ $_env == \'*\' || $_env == \"*\" ]] && _env=${_env:1:${#_env}-2}
		return 0
	done <.env
	return 1
}

# env_set NAME VALUE: NAME='VALUE' in .env, replacing its line or appended,
# through a temporary .env.<random> (mode 0600) moved over the old file, so
# no half-written .env is ever read.
env_set() {
	local line found=0
	tmp_file=$(mktemp .env.XXXXXXXX) || die "no temporary file beside .env"
	{
		if [[ -f .env ]]; then
			while IFS= read -r line || [[ -n $line ]]; do
				if [[ $line == "$1="* ]]; then
					found=1
					printf "%s='%s'\n" "$1" "$2"
				else
					printf '%s\n' "$line"
				fi
			done <.env
		fi
		((found)) || printf "%s='%s'\n" "$1" "$2"
	} >"$tmp_file" || die ".env was not written"
	mv -f -- "$tmp_file" .env || die ".env was not written"
	tmp_file=""
}

# override_set NEED FIELD KEY=VALUE...: compose.override.yaml regenerated
# whole from its "# setup: KEY=VALUE" lines, each KEY set to VALUE (an empty
# VALUE drops it), through agree; false when nothing changes. An override
# without the marker on its first line is the reader's own and is never
# touched: when it holds NEED, nothing changes; otherwise the step stops,
# asking for NEED in FIELD of the service, or, without NEED, to move it away.
# TLS (remote) mounts tls/, gives serve the certificate, publishes the host
# port on each address in place of compose.yaml's and maps the certificate's
# name to loopback, where the admin listener is.
override_set() {
	local -A set=()
	local -a keys=() hosts=() addrs=()
	local line k p need=$1 field=$2 change=""
	shift 2
	if [[ -f compose.override.yaml ]]; then
		if ! { IFS= read -r line && [[ $line == "$override_marker" ]]; } <compose.override.yaml; then
			[[ -n $need && $(<compose.override.yaml) == *"$need"* ]] && return 1
			die "compose.override.yaml was not written by setup.sh, so it is left as it is: ${need:+add \"$need\" to the $field of its stargaite service yourself, or }move it away and run this again"
		fi
		while IFS= read -r line; do
			[[ $line =~ ^'# setup: '([a-z_]+)=(.*)$ ]] || continue
			keys+=("${BASH_REMATCH[1]}")
			set[${BASH_REMATCH[1]}]=${BASH_REMATCH[2]}
		done <compose.override.yaml
	fi
	for p; do
		[[ ${set[${p%%=*}]-} == "${p#*=}" ]] || change+=" $p"
	done
	[[ -n $change ]] || return 1
	if [[ -f compose.override.yaml ]]; then
		agreed=0 # a replacement is confirmed although the recreate was
		agree "compose.override.yaml is rewritten with$change, keeping its other \"# setup:\" lines"
	else
		agree "writing compose.override.yaml with$change"
	fi
	for p; do
		k=${p%%=*}
		[[ -v "set[$k]" ]] || keys+=("$k")
		set[$k]=${p#*=}
	done
	[[ -n ${set[host_gateway]-} ]] && hosts+=(host.docker.internal:host-gateway)
	[[ -n ${set[tls]-} ]] && hosts+=("${set[tls_name]}:127.0.0.1")
	read -ra addrs <<<"${set[addresses]-}"
	tmp_file=$(mktemp compose.override.XXXXXXXX) || die "no temporary file beside compose.override.yaml"
	{
		printf '%s\n' "$override_marker"
		for k in "${keys[@]}"; do
			[[ -n ${set[$k]} ]] && printf '# setup: %s=%s\n' "$k" "${set[$k]}"
		done
		printf 'services:\n  stargaite:\n'
		((${#hosts[@]})) && printf '    extra_hosts:\n' && printf '      - "%s"\n' "${hosts[@]}"
		if [[ -n ${set[tls]-} ]]; then
			printf '    command: ["serve", "--listen", "0.0.0.0:4000", "--tls-cert", "/tls/cert.pem", "--tls-key", "/tls/key.pem"]\n    ports: !override\n'
			printf '      - "%s:${STARGAITE_PORT:-4000}:4000"\n' "${addrs[@]}"
			printf '    volumes:\n      - ./tls:/tls:ro\n'
			[[ -z ${set[tls_group]-} ]] || printf '    group_add:\n      - "%s"\n' "${set[tls_group]}"
		fi
	} >"$tmp_file" || die "compose.override.yaml was not written"
	mv -f -- "$tmp_file" compose.override.yaml || die "compose.override.yaml was not written"
	tmp_file=""
}

# tls_state: from the override's "# setup:" lines, how the script reaches the
# proxy (rule 7): base_url; tls_args, each curl's way to the certificate's
# name on loopback and the certificate it trusts; and admin, the variables
# each CLI call gets on docker compose exec, which stay out of the service's
# environment, where SSL_CERT_FILE would also root serve's provider
# connections; and published, the addresses remote last published on.
tls_state() {
	local line name="" ca=""
	base_url=http://127.0.0.1:$port/v1 tls_args=() admin=() published=""
	[[ -f compose.override.yaml ]] || return 0
	while IFS= read -r line; do
		[[ $line =~ ^'# setup: tls_name='(.*)$ ]] && name=${BASH_REMATCH[1]}
		[[ $line =~ ^'# setup: tls_ca='(.*)$ ]] && ca=${BASH_REMATCH[1]}
		[[ $line =~ ^'# setup: addresses='(.*)$ ]] && published=${BASH_REMATCH[1]}
	done <compose.override.yaml
	[[ -n $name ]] || return 0
	base_url=https://$name:$port/v1
	tls_args=(--resolve "$name:$port:127.0.0.1")
	admin=(-e "STARGAITE_ADMIN_URL=https://$name:4001")
	if [[ -n $ca ]]; then
		tls_args+=(--cacert "tls/$ca")
		admin+=(-e "SSL_CERT_FILE=/tls/$ca")
	fi
}

# field JSON ARGS...: jq -r ARGS over JSON given on standard input, never as
# an argument, since it may hold a client key.
field() {
	local json=$1
	shift
	printf '%s' "$json" | jq -r "$@"
}

# ctl ARGS...: the CLI in the container, standard input closed.
ctl() { docker compose exec -T "${admin[@]}" stargaite /stargaite "$@" </dev/null; }

# cli WORD WORD ARGS...: one CLI command in the container, with --json; the
# answer is in $reply. Its findings, warnings included, are printed; a failure
# also prints the CLI's message and returns 1. The CLI bounds its own wait:
# 90 s, or the server's deadline for a verify.
cli() {
	local code message
	reply=$(ctl "$1" "$2" --json "${@:3}")
	code=$?
	field "$reply" '(.findings // .error.findings // [])[] | "  \(.severity): \(.resource): \(.message)"' 2>/dev/null
	((code == 0)) && return 0
	message=$(field "$reply" '.error.message // empty' 2>/dev/null)
	say "stargaite $1 $2: ${message:-failed with exit $code}"
	return 1
}

# curl_auth HEADER VALUE ARGS...: curl, bounded to 30 s, with "HEADER: VALUE"
# in a curl config on standard input, so the value is in no argument; \ and "
# are escaped for the config's quoted string.
curl_auth() {
	local v=$2
	v=${v//\\/\\\\}
	v=${v//\"/\\\"}
	printf 'header = "%s: %s"\n' "$1" "$v" | curl -sS --max-time 30 -K - "${@:3}"
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

# check_machine: the root rule, then everything install needs, all reported
# at once with where to get it (nothing is ever installed here), then exit 1.
# The host port must be free on loopback unless it is the one .env names and
# the container runs already.
check_machine() {
	local -a missing=()
	local v
	root_rule
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
	command -v curl >/dev/null || missing+=("curl: see https://curl.se/download.html")
	command -v jq >/dev/null || missing+=("jq: see https://jqlang.org/download/")
	if v=$(docker compose ps -q 2>/dev/null) && [[ $port != "$env_port" || -z $v ]] && (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
		missing+=("a free host port: 127.0.0.1:$port is taken; give another with --port <n>")
	fi
	((${#missing[@]} == 0)) && return 0
	say "setup.sh needs, before anything else:"
	printf '  - %s\n' "${missing[@]}"
	exit 1
}

# wait_ready: /health/ready within 120 s, else the last 30 log lines, exit 1.
wait_ready() {
	local i end=$((SECONDS + 120))
	for ((i = 0; i < 120 && SECONDS < end; i++)); do
		curl -fsS --max-time 2 -o /dev/null "${tls_args[@]}" "${base_url%/v1}/health/ready" 2>/dev/null && return 0
		sleep 1
	done
	say "not ready within 120 s; the last 30 log lines:"
	docker compose logs --tail 30 stargaite
	((${#tls_args[@]} == 0)) || say "./setup.sh remote again repairs a remote that did not finish"
	exit 1
}

# compose_up ARGS...: docker compose up -d ARGS, then readiness. It is bounded
# to 20 minutes, and killed 10 s later if it outlives the stop, which docker
# ignores at a terminal; --foreground keeps it in the terminal's process
# group, so that Ctrl-C reaches it.
compose_up() {
	local start=$SECONDS
	if ! timeout --foreground -k 10 1200 docker compose up -d "$@"; then
		((SECONDS - start < 1200)) || die "docker compose up was not done within 20 minutes; its output is above"
		die "docker compose up failed or was interrupted; its output is above"
	fi
	wait_ready
}

# recreate WHY: the container recreated to read a changed .env or override,
# confirmed unless agree was, then readiness.
recreate() {
	((agreed)) || confirm "the container is recreated to $1: requests in flight are given time to finish, and the rate-limit windows restart"
	compose_up --no-build
}

# clone_version: STARGAITE_VERSION, the clone's git describe as make image
# takes it, dev without git; exported for the build.
clone_version() {
	command -v git >/dev/null && STARGAITE_VERSION=$(git describe --tags --always --dirty 2>/dev/null) || STARGAITE_VERSION=dev
	export STARGAITE_VERSION
}

# start: .env (mode 0600) with the host port, then the container, built with
# the clone's version when none exists; one that runs on the port .env named
# (4000 without the line) is kept, and a stopped one is started.
start() {
	local kept=""
	[[ -f .env ]] || (umask 077 && : >.env) || die ".env was not created"
	if ! env_get kept STARGAITE_PORT; then
		say "writing STARGAITE_PORT=$port to .env"
		env_set STARGAITE_PORT "$port"
	elif [[ $kept != "$port" ]]; then
		confirm "STARGAITE_PORT is replaced in .env, from $kept to $port"
		env_set STARGAITE_PORT "$port"
	fi
	clone_version
	if [[ -z $(docker compose ps -a -q) ]]; then
		say "building and starting version $STARGAITE_VERSION (the first build takes a few minutes)"
		compose_up --build
	elif [[ $port != "$env_port" ]]; then
		recreate "publish 127.0.0.1:$port"
	elif [[ -z $(docker compose ps -q) ]]; then
		say "starting the stopped container"
		compose_up --no-build
	else
		say "the container runs; kept"
		wait_ready
	fi
}

# reach VAR WHAT [HINT]: a URL on the host's loopback becomes one on
# host.docker.internal, as the container reaches the host, and
# --allow-private-network joins the caller's args; a plain-HTTP URL is
# offered it.
reach() {
	local -n _url=$1
	if [[ $_url =~ ^(https?://)(localhost|127\.[0-9.]+|\[::1\])(:[0-9]+)?(/.*)?$ ]]; then
		_url=${BASH_REMATCH[1]}host.docker.internal${BASH_REMATCH[3]}${BASH_REMATCH[4]}
		say "localhost inside the container is the container: $2 is reached at $_url, the host's bridge address, so it must listen beyond the host's loopback${3:+ ($3)}"
		args+=(--allow-private-network)
	elif [[ $_url == http://* || -v opt[allow-private-network] ]] && yesno allow-private-network "$_url is plain HTTP or a private address: allow it?"; then
		args+=(--allow-private-network)
	fi
}

# gateway URL: the host-gateway line in the override when URL is on
# host.docker.internal.
gateway() {
	[[ $1 == *//host.docker.internal* ]] && override_set host.docker.internal:host-gateway extra_hosts host_gateway=1
}

# step_provider: the provider, from a preset or another compatible service,
# added through the CLI before any secret is typed, which checks the variable
# name and the URL; then its credential into .env and, for a local service,
# the override, once the recreate that reads them is agreed; the container is
# recreated once. A provider that exists is kept.
step_provider() {
	local presets n choice existing preset url var billing args=()
	cli providers presets || exit 1
	presets=$reply
	cli providers list || exit 1
	existing=$(field "$reply" '[.providers[].name] | join(", ")')
	if [[ ! -v opt[provider] ]] && interactive; then
		say "providers:"
		field "$presets" '.presets | to_entries[] | "  \(.key + 1)) \(.value.name)" +
			(if .value.untested then ", untested" else "" end) + (if (.value.base_url // "") == "" then ", asks for its URL" else "" end)'
		n=$(field "$presets" '.presets | length')
		say "  $((n + 1))) another OpenAI- or Anthropic-compatible service"
		[[ -n $existing ]] && say "  already added: $existing"
		ask choice provider "" "provider: a number or a name"
		if [[ $choice =~ ^[0-9]+$ ]] && ((choice >= 1 && choice <= n)); then
			choice=$(field "$presets" --argjson i "$((choice - 1))" '.presets[$i].name')
		elif [[ $choice == "$((n + 1))" ]]; then
			ask choice provider "" "a name for the service"
		fi
		opt[provider]=$choice
	fi
	ask provider provider "" "the provider: a preset, a provider already added, or a new name"
	provider_json=$(field "$reply" --arg n "$provider" '.providers[] | select(.name == $n)')
	if [[ -n $provider_json ]]; then
		say "provider $provider exists; kept"
	else
		preset=$(field "$presets" --arg n "$provider" '.presets[] | select(.name == $n)')
		if [[ -n $preset ]]; then
			url=$(field "$preset" '.base_url // ""')
			[[ -n $url && ! -v opt[base-url] ]] || ask url base-url "" "the base URL of $provider"
		else
			say "$provider is not a preset: another OpenAI- or Anthropic-compatible service"
			ask protocol protocol openai "its protocol, openai or anthropic"
			ask url base-url "" "its base URL"
			var=${provider^^}
			ask var api-key-env "${var//[^A-Z0-9_]/_}_API_KEY" "the variable for its credential, or none"
			billing=none
			[[ $var != none ]] && billing=pay_per_use && args+=(--api-key-env "$var")
			args+=(--protocol "$protocol" --billing "$billing")
		fi
		reach url "$provider" "Ollama binds 127.0.0.1 unless OLLAMA_HOST=0.0.0.0"
		say "adding provider $provider"
		cli providers add --base-url "$url" "${args[@]}" -- "$provider" || exit 1
		provider_json=$(field "$reply" .resource)
	fi
	protocol=$(field "$provider_json" .protocol)
	var=$(field "$provider_json" '.api_key_env // ""')
	key_value=""
	[[ -n $var ]] && credentials "$var" key-env
	gateway "$(field "$provider_json" .base_url)"
	# A credential the container lacks, as after a run that stopped before
	# its recreate, recreates it too.
	if ((agreed)) || { cli providers show -- "$provider" && [[ $(field "$reply" .credential) == unresolved ]]; }; then
		recreate "read $var"
	fi
	cli providers show -- "$provider" || exit 1
	[[ $(field "$reply" .credential) =~ ^(resolved|none)$ ]] || die "the credential of $provider does not resolve: $var is not in the container's environment"
}

# list_models URL: the ids the provider lists, one per line, from one free
# GET of its model list, the credential in a header read from standard input.
# A failure, an empty list or an answer that is not one prints one line on
# standard error and nothing else.
list_models() {
	local answer ids
	if [[ $protocol == anthropic ]]; then
		answer=$(curl_auth x-api-key "$key_value" -H 'anthropic-version: 2023-06-01' --fail "${1%/}/v1/models?limit=1000" 2>/dev/null)
	elif [[ -n $key_value ]]; then
		answer=$(curl_auth Authorization "Bearer $key_value" --fail "${1%/}/models" 2>/dev/null)
	else
		answer=$(curl -sS --max-time 30 --fail "${1%/}/models" 2>/dev/null)
	fi || {
		say "the model list at $1 was not read; type the model id" >&2
		return
	}
	ids=$(field "$answer" '.data[].id' 2>/dev/null)
	[[ -n $ids ]] || {
		say "the model list at $1 is empty or not a list; type the model id" >&2
		return
	}
	printf '%s\n' "$ids"
}

# step_model: the model id, offered at a terminal from the provider's own
# list, read at the host-side URL and filtered by what the reader types.
step_model() {
	local url answer filter="" id i pick="" ids=() shown=()
	if [[ ! -v opt[model] ]] && interactive; then
		url=$(field "$provider_json" .base_url)
		mapfile -t ids < <(list_models "${url/\/\/host.docker.internal/\/\/localhost}")
		while ((${#ids[@]})) && [[ -z $pick ]]; do
			shown=()
			for id in "${ids[@]}"; do
				[[ $id == *"$filter"* ]] && shown+=("$id")
			done
			if ((${#shown[@]} == 0)); then
				say "$filter is not in the list; it is taken as typed"
				pick=$filter
				break
			fi
			for i in "${!shown[@]}"; do
				((i < 40)) && printf '  %d) %s\n' $((i + 1)) "${shown[i]}"
			done
			((${#shown[@]} > 40)) && say "  and $((${#shown[@]} - 40)) more: type part of an id to filter"
			read -r -p "model: a number, an id, or part of one to filter: " answer || die "no answer to: model"
			if [[ $answer =~ ^[0-9]+$ ]] && ((answer >= 1 && answer <= ${#shown[@]} && answer <= 40)); then
				pick=${shown[answer - 1]}
			fi
			for id in "${ids[@]}"; do
				[[ $id == "$answer" ]] && pick=$id
			done
			filter=$answer
		done
		[[ -n $pick ]] && opt[model]=$pick
	fi
	ask model model "" "the model id at $provider"
}

# step_target: the chat target provider/model, kept when it exists, then
# checked with targets verify unless it is verified and --reverify is not
# given: the paid calls counted and bounded first, never retried.
step_target() {
	local t calls total
	target=$provider/$model
	cli targets list || exit 1
	t=$(field "$reply" --arg id "$target" '.targets[] | select(.id == $id)')
	if [[ -n $t ]]; then
		say "target $target exists; kept"
	else
		say "adding target $target"
		cli targets add --kind chat -- "$target" || exit 1
		t=$(field "$reply" .resource)
	fi
	if [[ -n $(field "$t" '.verified_at // empty') && ! -v opt[reverify] ]]; then
		say "target $target is verified; not verified again (--reverify asks for it)"
		return
	fi
	calls=$(field "$t" '[(.reasoning.levels // [])[], (if .reasoning.can_disable then "none" else empty end)] | length | if . == 0 then 1 else . end')
	total=$(field "$provider_json" '.timeouts.total_ms // empty')
	if [[ -z $total ]]; then
		cli settings show || exit 1
		total=$(field "$reply" .timeouts.total_ms)
	fi
	say "targets verify $target: $calls paid call(s) to $provider, each capped at 256 output tokens, never retried; interrupted, the server still ends them within $((calls * (total / 1000 + 5))) s"
	if ! cli targets verify --max-output-tokens 256 -- "$target"; then
		field "$reply" '.calls[]? | "  \(.effort // "" | if . == "" then "call" else . end): status \(.status), \(.provider_message // .error.message // "no answer")"' 2>/dev/null
		die "target $target failed its check; no alias or key was created"
	fi
	say "target $target is verified"
}

# alias_kept: the alias's name; true, and said, when it exists: it is kept.
alias_kept() {
	local exists
	ask alias alias main "a name for the alias your clients call"
	cli models list || exit 1
	exists=$(field "$reply" --arg n "$alias" '.models[] | select(.name == $n) | .targets | join(", ")')
	[[ -n $exists ]] && say "alias $alias exists, toward $exists; kept"
}

# step_alias: the alias, which alias_kept found new, toward the target.
step_alias() {
	local max effort args=()
	if [[ $protocol == anthropic ]]; then
		ask max max-output-tokens 8192 "the alias's output cap in tokens, which an anthropic target needs"
		args+=(--max-output-tokens "$max")
	fi
	ask effort effort "" "a default reasoning effort, such as low, medium or high; empty for none" optional
	[[ -n $effort ]] && args+=(--effort "$effort" --effort-mode default)
	say "adding alias $alias toward $target"
	cli models add --target "$target" "${args[@]}" -- "$alias" || exit 1
}

# step_key: a client key allowed the alias, printed once inside a frame and
# written nowhere. A key that exists keeps its secret, which cannot be shown
# again: the reader names another, or the first answer is skipped (false),
# which, as for anything kept, is no failure.
step_key() {
	local rpm
	ask key_name key-name my-app "a name for the client key"
	cli keys list || exit 1
	while [[ -n $(field "$reply" --arg n "$key_name" '.keys[] | select(.name == $n) | .name') ]]; do
		say "key $key_name exists; its secret cannot be shown again"
		if ! interactive; then
			say "the first answer is skipped: give --key-name with a new name to make a key for it"
			return 1
		fi
		read -r -p "another name, or Enter to skip the first answer: " key_name && [[ -n $key_name ]] || {
			say "no key made; the first answer is skipped"
			return 1
		}
	done
	ask rpm rpm-limit 60 "the requests a minute the key may send"
	say "creating key $key_name for alias $alias"
	cli keys create --model "$alias" --rpm-limit "$rpm" -- "$key_name" || exit 1
	client_key=$(field "$reply" --arg n "$key_name" '.secrets[$n]')
	say "+-------------------------------------------------------------------"
	say "| client key $key_name, shown once and written nowhere: store it now"
	say "| $client_key"
	say "+-------------------------------------------------------------------"
}

# step_answer: one chat completion through the data plane with the new key.
# The headers come first in the answer (-D -), up to an empty line: curl's
# %header{} needs 7.84.0, newer than some supported systems ship.
step_answer() {
	local body out status code time served="" re=$'\nx-stargaite-target: *([^\r]*)'
	say "one test chat completion through the proxy: a paid call through alias $alias, capped at 256 output tokens, never retried"
	body=$(jq -nc --arg m "$alias" '{model: $m, max_tokens: 256, messages: [{role: "user", content: "Say hello in one short sentence."}]}')
	out=$(curl_auth Authorization "Bearer $client_key" -H 'Content-Type: application/json' -d "$body" \
		-D - -w '\n%{http_code} %{time_total}' "${tls_args[@]}" "$base_url/chat/completions") || {
		code=$?
		say "the test call did not complete (curl exit $code); the last 30 log lines:"
		docker compose logs --tail 30 stargaite
		exit 1
	}
	status=${out##*$'\n'}
	out=${out%$'\n'*}
	shopt -s nocasematch
	[[ ${out%%$'\r\n\r\n'*} =~ $re ]] && served=${BASH_REMATCH[1]}
	shopt -u nocasematch
	out=${out#*$'\r\n\r\n'}
	read -r code time <<<"$status"
	[[ $code == 2* ]] || die "the test call answered $code: $(field "$out" '.error.message // empty' 2>/dev/null)"
	say "answer: $(field "$out" '.choices[0].message.content // ""')"
	say "finish_reason: $(field "$out" '.choices[0].finish_reason // "-"'), $time s, from ${served:--}"
}

# machine: the host port, from --port or .env (4000 without either), the
# checks of check_machine and how the proxy is reached.
machine() {
	port=4000
	env_get port STARGAITE_PORT
	env_port=$port
	[[ -v opt[port] ]] && port=${opt[port]}
	if [[ ! $port =~ ^[1-9][0-9]{0,4}$ ]] || ((port > 65535)); then
		[[ -v opt[port] ]] || die "STARGAITE_PORT in .env is not a port number, 1 to 65535: write it as STARGAITE_PORT=<n>, or give --port"
		usage_error "--port takes a port number, 1 to 65535"
	fi
	check_machine
	tls_state
}

# running: a container that runs, then machine and readiness, which every
# subcommand but install, status and remote needs. Only Compose answering
# with none means none runs; a Docker or Compose that fails is named by
# machine.
running() {
	local id
	id=$(docker compose ps -q 2>/dev/null) && [[ -z $id ]] && die "no container runs here: ./setup.sh install starts it"
	machine
	wait_ready
}

# budget: a budget on the instance, an alias or a key; one that replaces
# another is confirmed. When the CLI refuses it because a target it reaches
# has no price (USE-12), the prices of exactly the targets it names are asked
# and set, and the budget applied once more.
budget() {
	local on limit reset in out today old id ids=() args=() show=(settings show) num='^[0-9]+(\.[0-9]+)?$'
	ask on on "" "what the budget is on: instance, alias:<name> or key:<name>"
	ask limit limit-usd "" "the limit in USD"
	ask reset reset monthly "when it resets: daily, monthly or never"
	[[ $limit =~ $num ]] || usage_error "--limit-usd takes a number of USD"
	[[ $reset =~ ^(daily|monthly|never)$ ]] || usage_error "--reset takes daily, monthly or never"
	case $on in
	instance) args=(settings set) ;;
	alias:?*) args=(models set) show=(models show -- "${on#*:}") ;;
	key:?*) args=(keys set) show=(keys show -- "${on#*:}") ;;
	*) usage_error "--on takes instance, alias:<name> or key:<name>" ;;
	esac
	args+=(--budget "limit_usd=$limit,reset_interval=$reset")
	[[ $on == instance ]] || args+=(-- "${on#*:}")
	cli "${show[@]}" || exit 1
	old=$(field "$reply" '.budget // empty | "\(.limit_usd) USD, reset \(.reset_interval)"')
	[[ -n $old ]] && confirm "the budget on $on, $old, is replaced"
	say "setting a budget of $limit USD on $on, reset $reset"
	cli "${args[@]}" && return
	mapfile -t ids < <(field "$reply" '(.findings // .error.findings // [])[] | select(.code == "unpriced_under_budget") | .resource | ltrimstr("targets/")' 2>/dev/null)
	((${#ids[@]})) || exit 1
	printf -v today '%(%Y-%m-%d)T' -1
	for id in "${ids[@]}"; do
		ask in input-price "" "the price of $id in USD per million input tokens"
		ask out output-price "" "the price of $id in USD per million output tokens"
		[[ $in =~ $num && $out =~ $num ]] || usage_error "--input-price and --output-price take a number of USD"
		say "setting the prices of $id to $in and $out USD per million input and output tokens, as of $today, which clears its verification: a later install or alias toward it verifies it again"
		cli targets set --pricing "input_per_million=$in,output_per_million=$out,as_of=$today" -- "$id" || exit 1
	done
	cli "${args[@]}" || exit 1
	say "the budget of $limit USD on $on is set"
}

# telemetry: Langfuse or any OTLP endpoint, its credentials into .env, a
# local endpoint reached as a local provider is. A block that exists is
# replaced whole, once confirmed: telemetry set is a merge patch, so an
# earlier backend's credentials would otherwise go to the new endpoint. Then
# telemetry set; the container recreated when this run changed .env or the
# override, or when a reference does not resolve, as after a run that stopped
# before its recreate; and telemetry test, its line printed as it comes.
telemetry() {
	local backend endpoint header var old creds=() args=()
	ask backend backend "" "the backend: langfuse or otlp"
	if [[ $backend == langfuse ]]; then
		[[ -v opt[header-name] || -v opt[key-env] ]] && usage_error "--header-name and --key-env are for --backend otlp"
		ask endpoint endpoint "" "the Langfuse endpoint, https://<host>/api/public/otel"
		args+=(--preset langfuse)
		creds=(LANGFUSE_PUBLIC_KEY langfuse-public-env LANGFUSE_SECRET_KEY langfuse-secret-env)
	elif [[ $backend == otlp ]]; then
		ask endpoint endpoint "" "the OTLP endpoint in full; spans go to <endpoint>/v1/traces"
		ask header header-name "" "a header that carries a credential, such as Authorization; empty for none" optional
		[[ -z $header && -v opt[key-env] ]] && usage_error "--key-env holds the credential of --header-name: give both"
		if [[ -n $header ]]; then
			var=OTLP_${header^^}
			var=${var//[^A-Z0-9_]/_}
			args+=(--header-env "$header=$var")
			creds=("$var" key-env)
		fi
	else
		usage_error "--backend takes langfuse or otlp"
	fi
	cli telemetry status || exit 1
	old=$(field "$reply" '.telemetry.otlp_endpoint // empty')
	[[ -n $old ]] && confirm "telemetry toward $old is replaced whole, none of its credentials or headers kept: it is unset first, so a change the CLI refuses leaves it unset"
	((${#creds[@]})) && credentials "${creds[@]}"
	reach endpoint telemetry
	gateway "$endpoint"
	if [[ -n $old ]]; then
		cli telemetry unset --yes || exit 1
	fi
	say "setting telemetry to $endpoint"
	cli telemetry set --otlp-endpoint "$endpoint" "${args[@]}" || die "telemetry is ${old:+now unset; it was toward }${old:-not set}"
	cli telemetry status || exit 1
	var=$(field "$reply" '.unresolved // [] | join(", ")')
	if ((agreed)) || [[ -n $var ]]; then
		recreate "read $var"
	fi
	say "telemetry test: one span to $endpoint"
	ctl telemetry test
}

# status: the running version and the clone's, and the state of each
# credential, of the providers and of telemetry; no more (cut 1 of the
# design's section 6). With --check, first whether the upstream has an
# upgrade, from one bounded git fetch.
status() {
	local v id
	clone_version
	if [[ -v opt[check] ]]; then
		fetch
		if ((ahead)); then
			say "upgrade: $ahead new commit(s) on $upstream; ./setup.sh upgrade applies them"
		else
			say "upgrade: none, up to date with $upstream"
		fi
	fi
	if id=$(docker compose ps -q 2>/dev/null) && [[ -z $id ]]; then
		say "version: $STARGAITE_VERSION in this clone; no container runs (./setup.sh install starts it)"
		return
	fi
	machine
	v=$(ctl version --json) || die "the running version was not read"
	say "version: $(field "$v" .version) running, $STARGAITE_VERSION in this clone"
	cli providers list || exit 1
	field "$reply" '.providers[] | "provider \(.name): credential \(.credential)"'
	cli telemetry status || exit 1
	field "$reply" 'if .telemetry.otlp_endpoint == null then "telemetry: not set" else "telemetry \(.telemetry.otlp_endpoint): " +
		(if .unresolved then "credentials unresolved: \(.unresolved | join(", "))" elif .telemetry.basic_auth_env // .telemetry.header_env
		then "credentials resolved" else "no credential" end) end'
}

# remote: the data plane published on loopback and the addresses given,
# always with TLS (SEC-6): the reader's certificate, or a self-signed leaf
# made here. Its files go to tls/, readable by the container's uid 65532: as
# root, owned by it; otherwise by the reader's primary group, which the
# override adds to the container, so rootless Docker and user-namespace
# remapping, where that group is another, are refused. serve reads the
# certificate only at start: run again, it renews it. It makes no CLI call
# before its own recreate, so a container that exists is enough, ready or
# not: run again, it also repairs a run that failed to start the container,
# or a certificate that no longer verifies. The addresses default to those
# published (rule 7), so a renewal keeps them.
remote() {
	local a m id host="" addresses choice name cert key ca="" cafile="" gid="" owner=65532:65532 n=0 san names out old="" addrs=() modes=(0700 0600)
	local o='(25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])'
	id=$(docker compose ps -a -q 2>/dev/null) && [[ -z $id ]] && die "no container here: ./setup.sh install makes it"
	machine
	[[ $(docker info --format '{{.SecurityOptions}}' 2>/dev/null) =~ name=(rootless|userns) ]] &&
		die "Docker runs rootless or with user-namespace remapping here, so the container's uid and groups are not this host's and tls/ cannot be made readable to it; nothing was changed"
	command -v hostname >/dev/null && host=$(hostname -I 2>/dev/null) && host=${host% }
	interactive && [[ -n $host ]] && say "this host's addresses: $host"
	a=${published#127.0.0.1}
	a=${a# }
	ask addresses address "${a// /,}" "the addresses to publish on beside 127.0.0.1, separated by commas; 127.0.0.1 alone for loopback only" optional
	IFS=', ' read -ra addrs <<<"$addresses"
	out=127.0.0.1
	for a in "${addrs[@]}"; do
		[[ $a =~ ^$o(\.$o){3}$ ]] || usage_error "--address takes IPv4 addresses of this host, such as 192.0.2.10, separated by commas: not $a"
		[[ " $out " == *" $a "* ]] && continue
		[[ -z $host || $a == 0.0.0.0 || " $host " == *" $a "* ]] || usage_error "$a is not an address of this host, which has $host"
		out+=" $a"
	done
	# A generated certificate names each address given; 0.0.0.0, which no
	# client dials, stands for this host's IPv4 addresses.
	names=$out
	[[ " $out " == *" 0.0.0.0 "* ]] && out=0.0.0.0 names+=" $host"
	read -ra addrs <<<"$out"
	ask choice tls "" "the certificate: own (yours, with its key) or self-signed (made here with openssl)"
	if [[ $choice == own ]]; then
		ask cert tls-cert "" "the absolute path of the certificate chain, in PEM"
		ask key tls-key "" "the absolute path of its private key, in PEM"
		ask name tls-name "" "the name on it that the admin CLI reaches it by"
		ask ca tls-ca "" "the absolute path of the certificate of the private CA that signed it; empty for a public CA" optional
		for a in "$cert" "$key" ${ca:+"$ca"}; do
			[[ $a == /* ]] || usage_error "--tls-cert, --tls-key and --tls-ca take absolute paths: not $a"
			[[ -f $a && -r $a ]] || die "$a is not a file setup.sh can read; nothing was changed"
		done
		[[ -n $ca ]] && cafile=ca.pem
	elif [[ $choice == self-signed ]]; then
		[[ -v opt[tls-cert] || -v opt[tls-key] || -v opt[tls-name] || -v opt[tls-ca] ]] &&
			usage_error "--tls-cert, --tls-key, --tls-name and --tls-ca are for --tls own"
		command -v openssl >/dev/null || die "openssl makes the self-signed certificate: see https://openssl-library.org/source/, or give yours with --tls own"
		[[ $out == 0.0.0.0 && -z $host ]] &&
			say "warning: this host's addresses are unknown here (no hostname -I), so the certificate names only those given beside 0.0.0.0 in --address"
		name=localhost cafile=cert.pem
	else
		usage_error "--tls takes own or self-signed"
	fi
	[[ $name =~ ^[A-Za-z0-9.-]+$ ]] || usage_error "--tls-name takes a host name, such as proxy.example.com"
	if [[ $(id -u) != 0 ]]; then
		gid=$(id -g) modes=(0750 0640)
		owner=:$gid
		if command -v getent >/dev/null; then
			while IFS=: read -r _ _ _ a _; do [[ $a == "$gid" ]] && ((n++)); done < <(timeout 10 getent passwd)
			# The reader's own name among the group's members is no other one.
			IFS=: read -r _ _ _ m < <(timeout 10 getent group "$gid") && m=,$m, && m=${m//,$(id -un),/,} && [[ -n ${m//,/} ]] && n=2
		fi
		((n > 1)) && say "warning: group $gid, which the container joins to read tls/, has other members, who can read its key too"
	fi
	say "tls/ gets the certificate and its key, and from now on the data plane speaks HTTPS only: every client, local ones too, needs https:// and to trust the certificate"
	[[ -f compose.override.yaml ]] && old=$(<compose.override.yaml)
	override_set "" "" "tls=$choice" "tls_name=$name" "tls_ca=$cafile" "tls_group=$gid" "addresses=${addrs[*]}" ||
		agree "the certificate and key in tls/ are replaced"
	if ! (
		umask 077
		mkdir -p tls || exit 1
		if [[ $choice == own ]]; then
			cp -- "$cert" tls/cert.pem && cp -- "$key" tls/key.pem && { [[ -z $ca ]] || cp -- "$ca" tls/ca.pem; }
		else
			san=DNS:localhost${HOSTNAME:+,DNS:$HOSTNAME}
			for a in $names; do
				[[ $a =~ ^$o(\.$o){3}$ && $a != 0.0.0.0 && $san, != *",IP:$a,"* ]] && san+=,IP:$a
			done
			out=$(openssl req -x509 -config <(printf '[req]\ndistinguished_name = dn\n[dn]\n') -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
				-keyout tls/key.pem -out tls/cert.pem -days 825 -subj /CN=localhost -addext "subjectAltName=$san" \
				-addext basicConstraints=critical,CA:FALSE -addext extendedKeyUsage=serverAuth 2>&1) || { say "$out"; exit 1; }
		fi
	) || ! chown -R "$owner" tls || ! chmod "${modes[0]}" tls || ! chmod "${modes[1]}" tls/*; then
		# The override put back as it was, so the next run reads the old state.
		if [[ -n $old ]]; then printf '%s\n' "$old" >compose.override.yaml; else rm -f -- compose.override.yaml; fi &&
			die "tls/ was not written, or not given its owner and modes; compose.override.yaml is as it was"
		die "tls/ was not written, and compose.override.yaml was not put back: move it away and run this again"
	fi
	tls_state
	# Compose recreates only a container whose configuration changed, and a
	# renewal changes none.
	(compose_up --no-build --force-recreate) || die "./setup.sh remote again${cert:+, with a certificate and key that match,} repairs it"
	for a in "${addrs[@]}"; do
		curl -fsS --max-time 30 -o /dev/null --resolve "$name:$port:$a" "${tls_args[@]:2}" "https://$name:$port/health/ready" ||
			die "the data plane did not answer over HTTPS on $a:$port"
		say "https on $a:$port: ready"
	done
	a=${addrs[*]}
	a=${a#127.0.0.1 }
	[[ $a == 0.0.0.0 ]] && a="this host"
	[[ $a == 127.0.0.1 ]] ||
		say "warning: Docker publishes past ufw and firewalld: every machine that reaches $a reaches port $port; limit it in your network's firewall or in the DOCKER-USER chain"
	command -v openssl >/dev/null && out=$(openssl x509 -enddate -noout -in tls/cert.pem 2>/dev/null) && say "the certificate expires on ${out#*=}; ./setup.sh remote again renews it"
	if [[ $choice == self-signed ]]; then
		say "clients trust this one certificate, tls/cert.pem copied to their machine, beside their own bundle: curl --cacert cert.pem;"
		say "  Python (requests, httpx) verify=\"cert.pem\"; Node.js NODE_EXTRA_CA_CERTS=cert.pem; or the system's trust store"
	fi
	say "base URL $base_url here; elsewhere, https://<an address the certificate holds>:$port/v1"
	say "the CLI from this directory: alias stargaite='docker compose exec -T ${admin[*]} stargaite /stargaite'"
}

# fetch: after the root rule, as git writes to the clone, the clone's branch
# and its upstream, fetched within 20 s without ever asking for a password
# (K19); ahead is how many commits the upstream has that HEAD lacks. A clone
# that is not one, a detached HEAD, a branch without upstream or a fetch that
# fails stops here, nothing changed.
fetch() {
	root_rule
	{ command -v git >/dev/null && git rev-parse --git-dir >/dev/null 2>&1; } ||
		die "upgrades come from a git clone of stargaite, with git: see https://git-scm.com/downloads"
	git symbolic-ref -q HEAD >/dev/null || die "HEAD is detached, so no branch says what to upgrade to: git switch <branch>, then run this again"
	upstream=$(git rev-parse --abbrev-ref '@{upstream}' 2>/dev/null) ||
		die "branch $(git branch --show-current) has no upstream to upgrade from: git branch --set-upstream-to <remote>/<branch>"
	GIT_TERMINAL_PROMPT=0 GIT_SSH_COMMAND='ssh -o BatchMode=yes' timeout -k 5 20 git fetch --quiet ||
		die "git fetch failed or did not finish within 20 s; nothing was changed"
	ahead=$(git rev-list --count 'HEAD..@{upstream}') || die "git rev-list failed; nothing was changed"
}

# upgrade: the clone fast-forwarded to its upstream and the image rebuilt;
# the running image kept as <image>-previous first, and a backup taken just
# before the new version starts, which is then checked. Nothing rolls back
# by itself, since a migration may be one-way: the way back is printed.
upgrade() {
	local old v valid image id backup n out line ready="" legacy="" failed=1
	running
	[[ -z $(git status --porcelain --untracked-files=no 2>/dev/null) ]] ||
		die "the clone has local changes to tracked files (git status lists them): commit or stash them, then run this again"
	fetch
	if ((ahead == 0)); then
		# An upgrade that did not finish leaves the clone at a commit that
		# does not run.
		{ out=$(ctl version --json) && v=$(field "$out" .version); } || die "the running version was not read"
		id=$(git rev-parse -q --verify "$v^{commit}" 2>/dev/null) && [[ $id != "$(git rev-parse HEAD)" ]] &&
			die "up to date with $upstream, but $v runs, not this clone's commit: an upgrade did not finish; git reset --hard $v, then run this again"
		say "up to date with $upstream; nothing was changed"
		return
	fi
	git merge-base --is-ancestor HEAD '@{upstream}' ||
		die "the clone has commits that $upstream lacks, so it cannot fast-forward: merge or rebase it yourself, then run this again"
	say "$ahead new commit(s) on $upstream:"
	git --no-pager log --oneline -20 'HEAD..@{upstream}'
	git diff --quiet HEAD '@{upstream}' -- internal/store/migrations ||
		say "a migration of the store changed: migrations are one-way, so the previous version cannot open the store they migrate"
	{ old=$(git rev-parse HEAD) && clone_version && out=$(ctl version --json) && v=$(field "$out" .version); } || die "the running version was not read"
	[[ $v == dev ]] && say "the running version is unknown (a dev build); this clone is at $STARGAITE_VERSION"
	[[ $v == dev || $v == "$STARGAITE_VERSION" ]] || say "the running version, $v, is not this clone's, $STARGAITE_VERSION"
	if [[ $(ctl --help) != *$'\n  restore '* ]]; then
		say "the running stargaite predates backup -, so setup.sh cannot take the backup: make it by hand, a copy that also stays in the volume,"
		say "  docker compose exec -T ${admin[*]:+${admin[*]} }stargaite /stargaite backup /data/pre-upgrade.db && mkdir -p backups && docker compose cp stargaite:/data/pre-upgrade.db backups/"
		ask legacy legacy-backup "" "the copy made by hand, on this host, such as backups/pre-upgrade.db"
		[[ -f $legacy && -s $legacy ]] || die "$legacy is not a file with a copy in it; nothing was changed"
	fi
	ctl validate --json >/dev/null
	valid=$?
	confirm "upgrading: the clone fast-forwards to $upstream and the image is rebuilt; then a backup of the store is taken and the container recreated on the new version, which interrupts traffic briefly"
	{ id=$(docker compose ps -q stargaite) && id=$(docker inspect --format '{{.Image}} {{.Config.Image}}' "$id") && image=${id#* } && id=${id%% *} &&
		docker tag "$id" "$image-previous"; } || die "the running image was not kept; nothing was changed"
	say "the running image is kept as $image-previous"
	git merge --ff-only --quiet '@{upstream}' || die "git merge --ff-only failed: $v still runs, the store unchanged"
	clone_version
	say "building version $STARGAITE_VERSION"
	timeout --foreground -k 10 1200 docker compose build ||
		die "the build failed or took over 20 minutes: $v still runs, the store unchanged; git reset --hard $old returns the clone to it"
	backup=$legacy
	if [[ -z $backup ]]; then
		TZ=UTC0 printf -v backup 'backups/%s-%(%Y%m%dT%H%M%SZ)T.db' "${v//[^A-Za-z0-9._-]/_}" -1
		say "backing up the store to $backup"
		out=$( (umask 077 && mkdir -p backups && ctl backup - --json >"$backup") 2>&1)
		# The summary is the last line, after anything docker itself printed;
		# a backup never started reports no count, and its file may not exist.
		n=$(field "${out##*$'\n'}" .bytes 2>/dev/null)
		if [[ ! $n =~ ^[1-9][0-9]*$ || $n != "$(stat -c %s "$backup" 2>/dev/null)" ]]; then
			rm -f -- "$backup"
			die "the backup failed ($out), so the new version was not started: $v still runs, the store unchanged; docker tag $image-previous $image and git reset --hard $old return to it"
		fi
	fi
	say "starting version $STARGAITE_VERSION"
	if (compose_up --no-build) && out=$(ctl version --json) && out=$(field "$out" .version); then
		ctl validate --json >/dev/null
		id=$?
		[[ $out == "$STARGAITE_VERSION" ]] || say "the running version is $out, not $STARGAITE_VERSION"
		((id == valid)) || say "validate exits $id, and $valid before: the new version refuses part of the stored configuration"
		while IFS= read -r line; do [[ $line == *'"msg":"ready"'* ]] && ready=$line; done < <(docker compose logs --no-log-prefix --tail 100 stargaite 2>/dev/null)
		[[ $out == "$STARGAITE_VERSION" ]] && ((id == valid)) && failed=0 && say "upgraded: ${ready:-running $out}"
	fi
	((failed)) && say "the upgrade failed. Nothing rolls back by itself, since a migration may be one-way."
	say "the way back to $v, from this directory, losing what changed since the backup; with the new image, as the previous one may predate restore:"
	say "  docker compose stop"
	say "  docker compose run --rm --no-deps -T stargaite restore --yes - < $(printf %q "$backup")"
	say "  docker tag $image-previous $image"
	say "  docker compose up -d --no-build"
	say "and, before any later build, git reset --hard $old, which setup.sh does not run"
	exit "$failed"
}

install() {
	machine
	start
	step_provider
	step_model
	step_target
	alias_kept || step_alias
	step_key && step_answer
	say ""
	say "Ready. Point an OpenAI-compatible client at base URL $base_url, with model $alias and its key, such as:"
	say "  curl ${tls_args[2]:+${tls_args[*]:2} }$base_url/chat/completions -H \"Authorization: Bearer sk-stargaite-...\" -d '{\"model\":\"$alias\",\"messages\":[{\"role\":\"user\",\"content\":\"Hello\"}]}'"
	say "Run ./setup.sh again to resume or check this installation; ./setup.sh help lists what it does."
}

usage() {
	printf '%s\n' "usage: ./setup.sh [subcommand] [flags]

The guided assistant: it asks only at a terminal, and only for what no flag
gave; without a terminal, a missing answer exits 2 naming its flag.

subcommands:
  install   check this machine, start the container, and go from a provider
            to a key and a first answer; run again, it resumes and keeps
            what exists (the default at a terminal before it exists; a menu
            of the others after)
  provider  add a provider and its credential to a running installation
  alias     an alias toward a provider's model, the target verified first;
            one that exists (--alias, main) is kept, with no paid call
  key       a client key for an alias (--alias, main), then a first answer
  budget    a spending limit on the instance, an alias or a key; the prices
            of the targets it reaches unpriced are asked first
  telemetry traces to Langfuse or any OTLP endpoint, replacing what was set
            whole, then telemetry test
  remote    publish the data plane beyond loopback, always with TLS: your
            certificate or a self-signed one; run again, it renews it
  status    the running and the clone's versions, each credential's state;
            with --check, whether the clone's upstream has an upgrade
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
  --model <id>              the model id at the provider
  --reverify                verify the target again although it is verified
  --alias <name>            the alias clients call (main)
  --max-output-tokens <n>   the alias's output cap, for an anthropic target (8192)
  --effort <level>          the alias's default reasoning effort (none)
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

telemetry flags, with --key-env, --replace, --allow-private-network and --yes:
  --backend <b>             langfuse or otlp
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
  --legacy-backup <file>    a copy made by hand, when the running version
                            predates backup -; setup.sh says how to make it

Paid calls: one targets verify (a call per reasoning level the target
declares) unless the target is verified, and one test chat completion, each
capped at 256 output tokens, announced first and never retried."
}

main() {
	if ((BASH_VERSINFO[0] < 4 || (BASH_VERSINFO[0] == 4 && BASH_VERSINFO[1] < 4))); then
		printf 'setup.sh: needs bash 4.4 or later; this is %s\n' "$BASH_VERSION" >&2
		exit 1
	fi
	trap cleanup EXIT
	trap 'exit 130' INT
	trap 'exit 143' TERM
	if [[ ${BASH_SOURCE[0]} == */* ]]; then
		cd -- "${BASH_SOURCE[0]%/*}" || exit 1
	fi
	[[ -f compose.yaml ]] || die "compose.yaml is not beside setup.sh: run it from the clone of stargaite"
	local sub=${1-} menu=(provider alias key budget telemetry status remote upgrade)
	local pf="provider base-url protocol api-key-env key-env" bf="replace allow-private-network yes"
	shift
	# With no subcommand at a terminal: install, or a menu once it exists.
	if [[ -z $sub ]]; then
		interactive || usage_error "no subcommand; ./setup.sh help lists them"
		sub=install
		if [[ -n $(docker compose ps -a -q 2>/dev/null) ]]; then
			say "this installation exists; ./setup.sh help says what each choice does:"
			say "  1) provider  2) alias  3) key  4) budget  5) telemetry  6) status  7) remote  8) upgrade"
			read -r -p "what to do: a number or a name: " sub || die "no answer to: what to do"
			[[ $sub =~ ^[1-8]$ ]] && sub=${menu[sub - 1]}
		fi
	fi
	case $sub in
	install) parse_flags "port $pf model alias max-output-tokens effort key-name rpm-limit" "$bf reverify" "$@"; install ;;
	provider) parse_flags "$pf" "$bf" "$@"; running; step_provider ;;
	alias) parse_flags "$pf model alias max-output-tokens effort" "$bf reverify" "$@"; running; alias_kept || { step_provider; step_model; step_target; step_alias; } ;;
	key) parse_flags "alias key-name rpm-limit" "" "$@"; running; alias_kept || die "alias $alias does not exist: ./setup.sh alias makes it"; if step_key; then step_answer; fi ;;
	budget) parse_flags "on limit-usd reset input-price output-price" "yes" "$@"; running; budget ;;
	telemetry) parse_flags "backend endpoint langfuse-public-env langfuse-secret-env header-name key-env" "$bf" "$@"; running; telemetry ;;
	remote) parse_flags "address tls tls-cert tls-key tls-name tls-ca" "yes" "$@"; remote ;;
	status) parse_flags "" "check" "$@"; status ;;
	upgrade) parse_flags "legacy-backup" "yes" "$@"; upgrade ;;
	help | --help | -h) usage ;;
	*) usage_error "unknown subcommand \"$sub\"; ./setup.sh help lists them" ;;
	esac
}

main "$@"; exit
