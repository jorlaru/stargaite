#!/usr/bin/env bash
# make setup-test: setup.sh end to end (docs/design/phase-6.md section 7), on
# a Linux host with Docker. A temporary copy of the working tree, made a git
# repository, runs ./setup.sh install, the console's first run, with every
# answer as a flag and no terminal, then key, budget, telemetry, remote with
# a self-signed certificate (then renewed), install again under TLS, status
# and, from one commit more on a bare
# upstream, upgrade, whose printed way back is then run, on that
# installation, under its own Compose project, image name and free host
# port, so it runs beside a service on port 4000. The mock provider runs
# from the source in the Go image, listening only on the Docker bridge's
# gateway address, and is reached as a local provider. It removes only the
# project, volume, images and container it created.
set -u

fail() {
	printf 'setup-test: %s\n' "$*" >&2
	exit 1
}

# free_port HOST...: a port nothing listens on at any HOST.
free_port() {
	local i p h
	for ((i = 0; i < 50; i++)); do
		p=$((20000 + RANDOM % 20000))
		for h; do
			(exec 3<>"/dev/tcp/$h/$p") 2>/dev/null && continue 2
		done
		echo "$p"
		return 0
	done
	return 1
}

# key_out NAME: what key printed for key NAME: its try announced, its
# result, and its secret once, alone on a line.
key_out() {
	[[ $out == *"try alias main: up to 1 paid call(s)"* ]] || fail "key $1: the try was not announced with its count"
	[[ $out == *"Done: key $1, which may call main."* ]] || fail "key $1: no result"
	[[ $(grep -cE '^sk-stargaite-[a-z2-7]{12}-[a-z2-7]{52}$' <<<"$out") == 1 ]] || fail "key $1: the secret is not printed once, alone on a line"
}

# again ARGS...: ./setup.sh ARGS in the copy, without a terminal; out holds
# what it printed, which is shown.
again() {
	out=$(cd "$tree" && ./setup.sh "$@" </dev/null 2>&1)
	code=$?
	printf '%s\n' "$out"
	((code == 0)) || fail "setup.sh $1 exited $code"
}

cleanup() {
	docker rm -f "$project-mock" >/dev/null 2>&1
	(cd "$tree" && docker compose down --volumes --remove-orphans >/dev/null 2>&1)
	docker image rm "$STARGAITE_IMAGE" "$STARGAITE_IMAGE-previous" ${built:+"$built"} >/dev/null 2>&1
	[[ $tree == */stargaite-setup-test.* ]] && rm -rf -- "$tree" "$tree.remote"
}

main() {
	local root gw mport port key want out code i cert a newer backup line keys adm chats
	built=""
	: "${GO_IMAGE:?GO_IMAGE names the Go image, as the Makefile sets it}"
	root=$(cd "$(dirname "$0")/../.." && pwd) || fail "no repository root"
	tree=$(mktemp -d "${TMPDIR:-/tmp}/stargaite-setup-test.XXXXXX") || fail "no temporary directory"
	project=stargaite-setup-test-${tree##*.}
	project=${project,,}
	export COMPOSE_PROJECT_NAME=$project STARGAITE_IMAGE=$project:local
	trap cleanup EXIT

	# The working tree as it is, ignored files left out, as a new repository.
	(cd "$root" && git ls-files -z -c -o --exclude-standard | tar --null --ignore-failed-read -T - -cf -) | tar -xf - -C "$tree" ||
		fail "the working tree was not copied"
	if ! { git -C "$tree" init -q && git -C "$tree" add -A &&
		git -C "$tree" -c user.name=setup-test -c user.email=setup-test@example.invalid commit -q -m "setup-test"; }; then
		fail "the copy was not made a repository"
	fi
	want=$(git -C "$tree" describe --tags --always --dirty) || fail "git describe"

	gw=$(docker run --rm --add-host gateway:host-gateway "$GO_IMAGE" getent hosts gateway) || fail "the bridge's gateway address was not found"
	gw=${gw%% *}
	mport=$(free_port "$gw") || fail "no free port at $gw"
	docker run -d --name "$project-mock" --network host --user "$(id -u):$(id -g)" -e HOME=/tmp -v "$tree:/src:ro" -w /src \
		"$GO_IMAGE" go run ./tests/mockprovider/cmd/mockprovider -listen "$gw:$mport" >/dev/null || fail "the mock provider did not start"
	for ((i = 0; i < 300; i++)); do
		curl -fsS --max-time 2 -o /dev/null "http://$gw:$mport/mock/calls" 2>/dev/null && break
		sleep 1
	done
	((i < 300)) || fail "the mock provider is not reachable at $gw:$mport: $(docker logs "$project-mock" 2>&1 | tail -5)"
	# Picked once the mock listens, free on both addresses remote publishes on.
	port=$(free_port 127.0.0.1 "$gw") || fail "no free port on loopback and $gw"

	# The credential holds $, " #", a space and an inner \, which .env must
	# carry byte for byte.
	# shellcheck disable=SC2016 # the $ is the point
	key='sk-e2e $HOME #x y\z'
	out=$(cd "$tree" && E2E_KEY=$key ./setup.sh install --port "$port" --provider mock --protocol openai \
		--base-url "http://localhost:$mport" --api-key-env MOCK_API_KEY --key-env E2E_KEY --model mock \
		--alias main --key-name e2e --yes </dev/null 2>&1)
	code=$?
	printf '%s\n' "$out"
	((code == 0)) || fail "setup.sh install exited $code"
	[[ $out == *"answer: mock reply"* ]] || fail "no first answer from the mock"
	[[ $out == *", from mock/mock"* ]] || fail "no x-stargaite-target of the first answer"
	key_out e2e
	[[ $out != *"$key"* ]] || fail "the credential was printed"
	[[ $(stat -c %a "$tree/.env") == 600 ]] || fail ".env is not mode 0600"
	grep -q 'host.docker.internal:host-gateway' "$tree/compose.override.yaml" || fail "the override has no host-gateway"
	[[ $(cd "$tree" && docker compose exec -T stargaite /stargaite version) == "$want" ]] || fail "the running version is not $want"
	# The verify call and the first answer each reached the mock with the
	# credential exactly as given.
	[[ $(curl -fsS --max-time 10 "http://$gw:$mport/mock/calls" | jq --arg a "Bearer $key" '[.[] | select(.Header.Authorization == [$a])] | length') -ge 2 ]] ||
		fail "the mock did not receive the credential byte for byte"

	# Run again: a key, a budget that prices the mock's target first, and
	# telemetry to the mock, first Langfuse, then OTLP with a header
	# credential, each needing a recreate.
	again key --alias main --key-name e2e-2
	[[ $out == *"answer: mock reply"* ]] || fail "no first answer through the second key"
	key_out e2e-2
	again budget --on alias:main --limit-usd 5 --input-price 1 --output-price 2
	[[ $out == *"setting the prices of mock/mock"* ]] || fail "the budget did not price mock/mock first"
	[[ $(cd "$tree" && docker compose exec -T stargaite /stargaite models show main --json | jq -c .budget) == '{"limit_usd":5,"reset_interval":"monthly"}' ]] ||
		fail "alias main has no budget of 5 USD"
	E2E_PK=pk-lf-e2e E2E_SK=sk-lf-e2e again telemetry --backend langfuse --endpoint "http://localhost:$mport" \
		--langfuse-public-env E2E_PK --langfuse-secret-env E2E_SK --yes
	[[ $out == *"span: accepted (200)"* && $out != *"-lf-e2e"* ]] || fail "the Langfuse test was not accepted, or a key was printed"
	# shellcheck disable=SC2016 # the $ is the point
	otlp='tok $HOME #x y\z'
	E2E_OTLP=$otlp again telemetry --backend otlp --endpoint "http://localhost:$mport" --header-name x-e2e-token --key-env E2E_OTLP --yes
	[[ $out == *"span: accepted (200)"* ]] || fail "the telemetry test was not accepted"
	[[ $out != *"$otlp"* ]] || fail "the telemetry credential was printed"
	# The last span carries the OTLP credential byte for byte, and nothing of
	# Langfuse's block, which the OTLP run replaced whole.
	[[ $(curl -fsS --max-time 10 "http://$gw:$mport/mock/calls" | jq --arg a "$otlp" '[.[] | select(.Path == "/v1/traces")] | last |
		.Header["X-E2e-Token"] == [$a] and .Header.Authorization == null and .Header["X-Langfuse-Ingestion-Version"] == null') == true ]] ||
		fail "the last span did not carry the OTLP credential alone"
	# Remote access with a self-signed certificate, published on loopback and
	# on the bridge's gateway address, which only this host reaches: HTTPS
	# with --cacert on both, plain HTTP no more, then a first answer and the
	# CLI (status below) over TLS.
	again remote --address "$gw" --tls self-signed --yes
	[[ $out == *"https on $gw:$port: ready"* && $out == *"the certificate expires on "* ]] || fail "remote did not check HTTPS on $gw:$port"
	for a in 127.0.0.1 "$gw"; do
		curl -fsS --max-time 10 -o /dev/null --cacert "$tree/tls/cert.pem" "https://$a:$port/health/ready" || fail "no HTTPS with --cacert on $a:$port"
	done
	if curl -fsS --max-time 10 -o /dev/null "http://127.0.0.1:$port/health/ready" 2>/dev/null; then
		fail "plain HTTP still answers on 127.0.0.1:$port"
	fi
	# Run again without --address, it renews: the addresses kept, and the
	# container recreated, so it serves the new certificate, which remote's
	# own HTTPS checks trust.
	cert=$(<"$tree/tls/cert.pem")
	again remote --tls self-signed --yes
	[[ $(<"$tree/tls/cert.pem") != "$cert" && $out == *"https on $gw:$port: ready"* ]] || fail "remote again did not renew the certificate on $gw:$port"
	# Add providers through the console, under TLS: the key checked for free
	# against the mock's model list, which it does not serve, so not
	# checked, then written, and the container recreated once.
	E2E_KEY=$key again provider --provider mock2 --protocol openai --base-url "http://$gw:$mport" --api-key-env MOCK2_API_KEY \
		--key-env E2E_KEY --allow-private-network --yes
	[[ $out == *"mock2: key not checked: http://$gw:$mport/models answered 404"* && $out == *"Done: mock2 added, key not checked."* ]] ||
		fail "Add providers did not check the key and finish"
	[[ $out != *"$key"* ]] || fail "the credential was printed"
	[[ $(curl -fsS --max-time 10 "http://$gw:$mport/mock/calls" | jq --arg a "Bearer $key" \
		'[.[] | select(.Path == "/models" and .Header.Authorization == [$a])] | length') == 1 ]] ||
		fail "the free check did not reach the mock with the key byte for byte"
	# Make an alias through the console, under TLS: budget priced mock/mock above, which cleared its
	# verification, so both targets are checked, through docker compose exec -T with a pipe as standard input.
	again alias --alias e2e-fb --provider mock --model mock --fallback mock2/mock
	adm=(-e STARGAITE_ADMIN_URL=https://localhost:4001 -e SSL_CERT_FILE=/tls/cert.pem)   # the admin API speaks TLS since remote
	[[ $out == *"check mock/mock: 1 paid call(s) to mock,"* && $out == *"check mock2/mock: 1 paid call(s) to mock2,"* &&
		$out == *"2 paid call(s) in all"* && $out == *"Done: alias e2e-fb toward mock/mock, mock2/mock."* ]] ||
		fail "Make an alias did not check both targets and finish"
	[[ $(cd "$tree" && docker compose exec -T "${adm[@]}" stargaite /stargaite models show e2e-fb --json | jq -c .targets) == '["mock/mock","mock2/mock"]' ]] ||
		fail "alias e2e-fb is not toward mock/mock, mock2/mock"
	for t in mock/mock mock2/mock; do
		[[ $(cd "$tree" && docker compose exec -T "${adm[@]}" stargaite /stargaite targets show "$t" --json | jq '.verified_at > 0') == true ]] ||
			fail "$t was not verified"
	done
	# Layer 4 against this container (phase-7.md section 10): the console's terminal tests, built in the Go image since
	# the host has no go, then run on the host: Esc during a delayed check on the mock, and no frame left in the history
	# of tmux or screen (skipped, saying why, when neither is installed).
	docker run --rm --user "$(id -u):$(id -g)" -e HOME=/tmp -v "$tree:/src" -w /src -e CGO_ENABLED=0 "$GO_IMAGE" \
		go test -c -o bin/console-pty.test ./cmd/stargaite-console || fail "the layer-4 test binary was not built"
	STARGAITE_E2E_TREE=$tree STARGAITE_E2E_MOCK=http://$gw:$mport "$tree/bin/console-pty.test" \
		-test.run '^(TestRealEscDuringCheck|TestFramesNotInHistory)$' -test.v || fail "the layer-4 tests against the real container failed"
	again key --alias main --key-name e2e-3
	[[ $out == *"answer: mock reply"* ]] || fail "no first answer over HTTPS"
	key_out e2e-3
	grep -qE 'curl --cacert tls/[^ ]+ https://[^ ]+/v1/chat/completions' <<<"$out" || fail "no example call over HTTPS with the CA"
	# install again under TLS: the Machine step reads the override and checks
	# readiness over HTTPS with the CA; the provider, alias and key are kept,
	# so no key is made and nothing paid reaches the mock.
	chats=$(curl -fsS --max-time 10 "http://$gw:$mport/mock/calls" | jq '[.[] | select(.Path == "/chat/completions")] | length')
	again install --port "$port" --provider mock --model mock --alias main --key-name e2e-3
	[[ $out == *"the container runs; kept"* && $out == *"alias main exists"* &&
		$out == *"key e2e-3 exists, and its secret cannot be shown again"* ]] || fail "install again under TLS did not keep what exists"
	[[ $(curl -fsS --max-time 10 "http://$gw:$mport/mock/calls" | jq '[.[] | select(.Path == "/chat/completions")] | length') == "$chats" ]] ||
		fail "install again under TLS made a paid call"
	again status
	[[ $out == *"version: $want running, $want in this clone"* && $out == *"provider mock: credential resolved"* &&
		$out == *"credentials resolved"* ]] || fail "status does not show the versions and the credentials"
	[[ $("$tree/bin/stargaite-console" --version) == "$want" ]] || fail "the console exported is not this clone's version"
	# Upgrade: the copy's upstream, a bare repository, holds one commit more.
	# upgrade keeps the previous image, builds, backs up, starts and checks
	# the new version; then the way back it printed is run as printed, and
	# the previous image serves the restored store, without the key made
	# after the backup. The commit moves every line of setup.sh, which the
	# merge replaces while it runs (main "$@"; exit).
	{ git init -q --bare "$tree.remote" && git -C "$tree" remote add origin "$tree.remote" && git -C "$tree" push -q -u origin HEAD &&
		sed -i '1a # setup-test upgrade: every line below moves' "$tree/setup.sh" &&
		git -C "$tree" -c user.name=setup-test -c user.email=setup-test@example.invalid commit -q -a -m "setup-test upgrade" &&
		newer=$(git -C "$tree" describe --tags --always --dirty) && git -C "$tree" push -q && git -C "$tree" reset -q --hard HEAD~1; } ||
		fail "the upstream with one commit more was not made"
	again upgrade --yes
	built=$(docker image inspect -f '{{.Id}}' "$STARGAITE_IMAGE") || fail "no image $STARGAITE_IMAGE after the upgrade"
	[[ $out == *'upgraded: {'*'"msg":"ready"'*"\"version\":\"$newer\""* ]] || fail "upgrade did not report version $newer ready"
	[[ $(cd "$tree" && docker compose exec -T stargaite /stargaite version) == "$newer" ]] || fail "the running version is not $newer"
	docker image inspect "$STARGAITE_IMAGE-previous" >/dev/null || fail "no previous image $STARGAITE_IMAGE-previous"
	[[ $out =~ "backing up the store to "(backups/[^ ]+\.db) ]] || fail "no backup named"
	backup=${BASH_REMATCH[1]}
	[[ $(stat -c %a "$tree/$backup") == 600 && -s $tree/$backup ]] || fail "$backup is empty or not mode 0600"
	(cd "$tree" && docker compose exec -T "${adm[@]}" stargaite /stargaite keys create after --model main --json >/dev/null) ||
		fail "no key made after the backup"
	while IFS= read -r line; do
		[[ $line == "  docker "* ]] || continue
		printf 'the way back: %s\n' "${line#  }"
		(cd "$tree" && eval "$line") || fail "the way back failed at: $line"
	done <<<"$out"
	for ((i = 0; i < 120; i++)); do
		curl -fsS --max-time 2 -o /dev/null --cacert "$tree/tls/cert.pem" "https://127.0.0.1:$port/health/ready" 2>/dev/null && break
		sleep 1
	done
	((i < 120)) || fail "the previous image is not ready after the restore"
	[[ $(cd "$tree" && docker compose exec -T stargaite /stargaite version) == "$want" ]] || fail "the previous image, $want, does not run"
	keys=$(cd "$tree" && docker compose exec -T "${adm[@]}" stargaite /stargaite keys list --json | jq -r '[.keys[].name] | sort | join(",")')
	[[ $keys == e2e,e2e-2,e2e-3 ]] || fail "the restored store holds the keys $keys, not e2e, e2e-2 and e2e-3"
	echo "setup-test: passed"
}

main "$@"
