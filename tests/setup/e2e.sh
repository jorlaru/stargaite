#!/usr/bin/env bash
# make setup-test: setup.sh end to end (docs/design/phase-6.md section 7), on
# a Linux host with Docker. A temporary copy of the working tree, made a git
# repository, runs ./setup.sh install with every answer as a flag and no
# terminal, then key, budget, telemetry, remote with a self-signed
# certificate (then renewed) and status on that installation, under its own
# Compose project, image name and free host port, so it runs beside a
# service on port 4000. The mock provider runs from the source in the Go
# image, listening only on the Docker bridge's gateway address, and is
# reached as a local provider. It removes only the project, volume, images
# and container it created.
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
	docker image rm "$STARGAITE_IMAGE" >/dev/null 2>&1
	[[ $tree == */stargaite-setup-test.* ]] && rm -rf -- "$tree"
}

main() {
	local root gw mport port key want out code i cert a
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
	again key --alias main --key-name e2e-3
	[[ $out == *"answer: mock reply"* ]] || fail "no first answer over HTTPS"
	again status
	[[ $out == *"version: $want running, $want in this clone"* && $out == *"provider mock: credential resolved"* &&
		$out == *"credentials resolved"* ]] || fail "status does not show the versions and the credentials"
	echo "setup-test: passed"
}

main "$@"
