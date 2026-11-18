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
tmp_file=""
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

# secret VAR NAME: the value for the credential variable NAME, from the
# variable of this script's environment that --key-env names, or typed hidden
# at a terminal. Only builtins touch it. .env holds it single-quoted, so a
# value with ' or a newline, or ending in \, is refused.
secret() {
	local -n _secret=$1
	local from=${opt[key-env]-}
	if [[ -n $from ]]; then
		# Not printed: a pasted value instead of a name would reach the screen.
		# Only an exported variable counts, never one of this script's own.
		[[ $from =~ ^[A-Za-z_][A-Za-z0-9_]*$ && $(declare -p -- "$from" 2>/dev/null) =~ ^declare\ -[a-zA-Z]*x ]] ||
			usage_error "--key-env takes the name of a variable exported to this script"
		_secret=${!from}
	elif interactive; then
		read -rs -p "$2 (typed hidden): " _secret || die "no answer to: $2"
		printf '\n'
	else
		usage_error "missing --key-env: the variable of this script's environment that holds $2"
	fi
	[[ -n $_secret ]] || die "$2 is empty"
	[[ $_secret != *"'"* && $_secret != *$'\n'* && $_secret != *\\ ]] ||
		die "$2 holds ' or a newline, or ends in \\: .env cannot hold it, so nothing was written"
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

# override_set KEY VALUE NEED FIELD: compose.override.yaml regenerated whole
# from its "# setup: KEY=VALUE" lines, with KEY set to VALUE; false when
# nothing changes. An override without the marker on its first line is the
# reader's own and is never touched: when it holds NEED, nothing changes;
# otherwise the step stops, asking for NEED in FIELD of the service.
override_set() {
	local -A set=()
	local -a keys=()
	local line k
	if [[ -f compose.override.yaml ]]; then
		if ! { IFS= read -r line && [[ $line == "$override_marker" ]]; } <compose.override.yaml; then
			[[ $(<compose.override.yaml) == *"$3"* ]] && return 1
			die "compose.override.yaml was not written by setup.sh, so it is left as it is: add \"$3\" to the $4 of its stargaite service yourself, or move it away and run this again"
		fi
		while IFS= read -r line; do
			[[ $line =~ ^'# setup: '([a-z_]+)=(.*)$ ]] || continue
			keys+=("${BASH_REMATCH[1]}")
			set[${BASH_REMATCH[1]}]=${BASH_REMATCH[2]}
		done <compose.override.yaml
		[[ ${set[$1]-} == "$2" ]] && return 1
		confirm "compose.override.yaml is rewritten with $1=$2, keeping its other \"# setup:\" lines"
	else
		say "writing compose.override.yaml with $1=$2"
	fi
	[[ -v "set[$1]" ]] || keys+=("$1")
	set[$1]=$2
	tmp_file=$(mktemp compose.override.XXXXXXXX) || die "no temporary file beside compose.override.yaml"
	{
		printf '%s\n' "$override_marker"
		for k in "${keys[@]}"; do
			printf '# setup: %s=%s\n' "$k" "${set[$k]}"
		done
		printf 'services:\n  stargaite:\n'
		[[ -n ${set[host_gateway]-} ]] && printf '    extra_hosts:\n      - "host.docker.internal:host-gateway"\n'
	} >"$tmp_file" || die "compose.override.yaml was not written"
	mv -f -- "$tmp_file" compose.override.yaml || die "compose.override.yaml was not written"
	tmp_file=""
}

# field JSON ARGS...: jq -r ARGS over JSON given on standard input, never as
# an argument, since it may hold a client key.
field() {
	local json=$1
	shift
	printf '%s' "$json" | jq -r "$@"
}

# cli WORD WORD ARGS...: one CLI command in the container, with --json; the
# answer is in $reply. Its findings, warnings included, are printed; a failure
# also prints the CLI's message and returns 1. The CLI bounds its own wait:
# 90 s, or the server's deadline for a verify.
cli() {
	local code message
	reply=$(docker compose exec -T stargaite /stargaite "$1" "$2" --json "${@:3}" </dev/null)
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

# check_machine: the root rule, then everything install needs, all reported
# at once with where to get it (nothing is ever installed here), then exit 1.
# The host port must be free on loopback unless it is the one .env names and
# the container runs already.
check_machine() {
	local -a missing=()
	local v
	if [[ $(id -u) == 0 && $(stat -c %u .) != 0 ]]; then
		die "run as root in a clone that belongs to another user, setup.sh would leave root-owned files (.env, tls/, backups/, git objects) that break its next run and that user's git: run it as the clone's owner"
	fi
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
	fi
	command -v curl >/dev/null || missing+=("curl: see https://curl.se/download.html")
	command -v jq >/dev/null || missing+=("jq: see https://jqlang.org/download/")
	if [[ $port != "$env_port" || -z $(docker compose ps -q 2>/dev/null) ]] && (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
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
		curl -fsS --max-time 2 -o /dev/null "http://127.0.0.1:$port/health/ready" 2>/dev/null && return 0
		sleep 1
	done
	say "not ready within 120 s; the last 30 log lines:"
	docker compose logs --tail 30 stargaite
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
# once confirmed, then readiness.
recreate() {
	confirm "the container is recreated to $1: requests in flight are given time to finish, and the rate-limit windows restart"
	compose_up --no-build
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
	command -v git >/dev/null && STARGAITE_VERSION=$(git describe --tags --always --dirty 2>/dev/null) || STARGAITE_VERSION=dev
	export STARGAITE_VERSION
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

# step_provider: the provider, from a preset or another compatible service,
# added through the CLI before any secret is typed, which checks the variable
# name and the URL; then its credential into .env and, for a local service,
# the override; the container is recreated once to read them. A provider that
# exists is kept.
step_provider() {
	local presets n choice existing preset url var billing change="" args=()
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
		if [[ $url =~ ^(https?://)(localhost|127\.[0-9.]+|\[::1\])(:[0-9]+)?(/.*)?$ ]]; then
			url=${BASH_REMATCH[1]}host.docker.internal${BASH_REMATCH[3]}${BASH_REMATCH[4]}
			say "localhost inside the container is the container: $provider is reached at $url, the host's bridge address, so it must listen beyond the host's loopback (Ollama binds 127.0.0.1 unless OLLAMA_HOST=0.0.0.0)"
			args+=(--allow-private-network)
		elif [[ $url == http://* || -v opt[allow-private-network] ]] && yesno allow-private-network "$url is plain HTTP or a private address: allow it?"; then
			args+=(--allow-private-network)
		fi
		say "adding provider $provider"
		cli providers add --base-url "$url" "${args[@]}" -- "$provider" || exit 1
		provider_json=$(field "$reply" .resource)
	fi
	protocol=$(field "$provider_json" .protocol)
	var=$(field "$provider_json" '.api_key_env // ""')
	key_value=""
	if [[ -n $var ]]; then
		if ! env_get key_value "$var" || { yesno replace "$var is in .env: replace it?" && key_value=""; }; then
			secret key_value "$var"
			say "writing $var to .env"
			env_set "$var" "$key_value"
			change="read $var"
		else
			say "$var is in .env; kept"
		fi
	fi
	if [[ $(field "$provider_json" .base_url) == *//host.docker.internal* ]] &&
		override_set host_gateway 1 host.docker.internal:host-gateway extra_hosts; then
		change+="${change:+ and }reach host.docker.internal"
	fi
	if [[ -z $change ]] && cli providers show -- "$provider" && [[ $(field "$reply" .credential) == unresolved ]]; then
		change="read $var"
	fi
	[[ -n $change ]] && recreate "$change"
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

# step_alias: the alias toward the target, or the one that exists, kept.
step_alias() {
	local exists max effort args=()
	ask alias alias main "a name for the alias your clients call"
	cli models list || exit 1
	exists=$(field "$reply" --arg n "$alias" '.models[] | select(.name == $n) | .targets | join(", ")')
	if [[ -n $exists ]]; then
		say "alias $alias exists, toward $exists; kept"
		return
	fi
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
# again: the reader names another, or the first answer is skipped (false).
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
		read -r -p "another name, or Enter to skip the first answer: " key_name && [[ -n $key_name ]] || return 1
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
		-D - -w '\n%{http_code} %{time_total}' "$base_url/chat/completions") || {
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

install() {
	port=4000
	env_get port STARGAITE_PORT
	env_port=$port
	[[ -v opt[port] ]] && port=${opt[port]}
	if [[ ! $port =~ ^[1-9][0-9]{0,4}$ ]] || ((port > 65535)); then
		[[ -v opt[port] ]] || die "STARGAITE_PORT in .env is not a port number, 1 to 65535: write it as STARGAITE_PORT=<n>, or give --port"
		usage_error "--port takes a port number, 1 to 65535"
	fi
	check_machine
	start
	step_provider
	step_model
	step_target
	step_alias
	base_url=http://127.0.0.1:$port/v1
	step_key && step_answer
	say ""
	say "Ready. Point an OpenAI-compatible client at base URL $base_url, with model $alias and its key, such as:"
	say "  curl $base_url/chat/completions -H \"Authorization: Bearer sk-stargaite-...\" -d '{\"model\":\"$alias\",\"messages\":[{\"role\":\"user\",\"content\":\"Hello\"}]}'"
	say "Run ./setup.sh again to resume or check this installation; ./setup.sh help lists what it does."
}

usage() {
	printf '%s\n' "usage: ./setup.sh [subcommand] [flags]

The guided assistant: it asks only at a terminal, and only for what no flag
gave; without a terminal, a missing answer exits 2 naming its flag.

subcommands:
  install   check this machine, start the container, and go from a provider
            to a key and a first answer; run again, it resumes and keeps
            what exists (the default at a terminal)
  help      this text

install flags:
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
	case ${1-} in
	install)
		shift
		parse_flags "port provider base-url protocol api-key-env key-env model alias max-output-tokens effort key-name rpm-limit" \
			"replace allow-private-network reverify yes" "$@"
		install
		;;
	help | --help | -h) usage ;;
	"")
		interactive || usage_error "no subcommand; ./setup.sh help lists them"
		install
		;;
	*) usage_error "unknown subcommand ${1-}; ./setup.sh help lists them" ;;
	esac
}

main "$@"; exit
