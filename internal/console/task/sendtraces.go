package task

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/jorlaru/stargaite/internal/console/host"
	"github.com/jorlaru/stargaite/internal/console/proxy"
	"github.com/jorlaru/stargaite/internal/console/ui"
)

// traceSteps is Send traces' step table: the IDs are those the steps it
// shares with Add providers ask at.
func traceSteps() []ui.Step {
	return []ui.Step{{ID: "which", Title: "where"}, {ID: "add", Title: "endpoint"}, {ID: "key", Title: "keys"},
		{ID: "write", Title: "write"}, {ID: "recreate", Title: "recreate"}, {ID: "test", Title: "test"}}
}

// traces is where a run sends traces: the endpoint as the container reaches
// it, the flags of telemetry set, and the credentials' variables.
type traces struct {
	endpoint string
	flags    []string
	creds    []credential
}

// credential is a variable for .env and the flag that names its value.
type credential struct{ variable, flag string }

// SendTraces sends the proxy's traces to Langfuse or another OTLP endpoint,
// or turns them off. Every question is answered before anything is written,
// a local endpoint handled as a local provider is; then the credentials go
// to .env, the block set is replaced whole, the container is recreated when
// it must read them, and one test span is sent, which calls no provider.
func SendTraces(ctx context.Context, u *ui.UI, px *proxy.Proxy, h *host.Host) (string, error) {
	a := &adding{u: u, px: px, h: h}
	if err := ready(ctx, u, h, "which"); err != nil {
		return "", err
	}
	backend, err := u.Pick(ctx, ui.Question{ID: "backend", Flag: "backend", Step: "which", Prompt: "Where do traces go?", Options: []ui.Option{
		{Value: "langfuse", Label: "Langfuse"}, {Value: "otlp", Label: "another OTLP endpoint"}, {Value: "off", Label: "turn traces off"}}})
	if err != nil {
		return "", err
	}
	if backend == "off" {
		return traceOff(ctx, a)
	}
	t, err := askTraces(ctx, a, backend)
	if err != nil {
		return "", err
	}
	old, _, err := telemetryNow(ctx, px)
	if err != nil {
		return "", err
	}
	if old != "" {
		if err := confirm(ctx, u, "add", "telemetry toward "+old+" is replaced whole, none of its credentials or headers kept: "+
			"it is unset first, so a change the CLI refuses leaves it unset"); err != nil {
			return "", err
		}
	}
	var writes []envWrite
	var kept []string
	for _, c := range t.creds {
		keep, err := a.keep(ctx, c.variable)
		switch {
		case err != nil:
			return "", err
		case keep:
			kept = append(kept, c.variable)
			continue
		}
		v, err := u.Secret(ctx, ui.Question{ID: c.flag, Flag: c.flag, Step: "key", Prompt: c.variable})
		if err != nil {
			return "", err
		}
		writes = append(writes, envWrite{name: c.variable, value: v, by: "telemetry"})
	}
	wrote, err := a.write(ctx, []string{t.endpoint}, writes)
	if err != nil {
		return "", err
	}
	if err := keptRecreate(ctx, a, kept); err != nil {
		return "", err
	}
	if err := setTraces(ctx, a, old, t); err != nil {
		return "", err
	}
	_, unresolved, err := telemetryNow(ctx, px)
	if err != nil {
		return "", err
	}
	if err := a.recreate(ctx, unresolved, wrote); err != nil {
		return "", err
	}
	return testSpan(ctx, a, t.endpoint)
}

// askTraces asks where traces go for backend, langfuse or otlp: the
// endpoint, and for otlp a header that carries a credential. A local
// endpoint moves to the host's bridge address, said.
func askTraces(ctx context.Context, a *adding, backend string) (traces, error) {
	header := ui.Question{ID: "header-name", Flag: "header-name", Step: "add",
		Prompt: "a header that carries a credential, such as Authorization; empty for none", HasDefault: true}
	keyEnv := ui.Question{ID: "key-env"}
	q := ui.Question{ID: "endpoint", Flag: "endpoint", Step: "add", Prompt: "the OTLP endpoint in full; spans go to <endpoint>/v1/traces", Check: noUserinfo}
	var t traces
	if backend == "langfuse" {
		if a.u.Given(header) || a.u.Given(keyEnv) {
			return t, &ui.UsageError{Msg: "--header-name and --key-env are for --backend otlp"}
		}
		q.Prompt = "the Langfuse endpoint, https://<host>/api/public/otel"
		t.flags = []string{"--preset", "langfuse"}
		t.creds = []credential{{"LANGFUSE_PUBLIC_KEY", "langfuse-public-env"}, {"LANGFUSE_SECRET_KEY", "langfuse-secret-env"}}
	}
	ep, err := a.u.Text(ctx, q)
	if err != nil {
		return t, err
	}
	if backend == "otlp" {
		h, err := a.u.Text(ctx, header)
		switch {
		case err != nil:
			return t, err
		case h == "" && a.u.Given(keyEnv):
			return t, &ui.UsageError{Msg: "--key-env holds the credential of --header-name: give both"}
		case h != "":
			v := "OTLP_" + variableOf(h, "")
			t.flags = []string{"--header-env", h + "=" + v}
			t.creds = []credential{{v, "key-env"}}
		}
	}
	ep, private, err := a.reach(ctx, "telemetry", ep, "")
	if err != nil {
		return t, err
	}
	t.endpoint, t.flags = ep, append([]string{"--otlp-endpoint", ep}, t.flags...)
	if private {
		t.flags = append(t.flags, "--allow-private-network")
	}
	return t, nil
}

// noUserinfo refuses an endpoint with a user or a password, never quoting
// it: its credentials are keys, asked hidden. Any "@" is refused, so an
// endpoint url.Parse misreads (a "/" in the password, no scheme) cannot
// carry one to the screen or an argv.
func noUserinfo(a string) error {
	if strings.Contains(a, "@") {
		return errors.New("the endpoint must not hold a user or a password: its credentials are asked as keys, hidden")
	}
	return nil
}

// telemetryNow is where traces go now, "" for nowhere, and the variables of
// their credentials that do not resolve in the container.
func telemetryNow(ctx context.Context, px *proxy.Proxy) (string, []string, error) {
	var st proxy.TelemetryReply
	if err := px.Call(ctx, &st, "telemetry status", nil); err != nil || st.Telemetry == nil {
		return "", nil, err
	}
	return st.Telemetry.OTLPEndpoint, st.Unresolved, nil
}

// setTraces unsets the block toward old, if any, then sets t's: telemetry
// set is a merge patch, so the old block's credentials would otherwise go to
// the new endpoint. A set the CLI refuses says where telemetry is left, and
// the refusal's findings. Only telemetry's own findings, on settings, are
// said: the answer holds those of the whole configuration.
func setTraces(ctx context.Context, a *adding, old string, t traces) error {
	var change proxy.Change
	if old != "" {
		if err := a.px.Call(ctx, &change, "telemetry unset", []string{"--yes"}); err != nil {
			return err
		}
	}
	say(ctx, a.u, "setting telemetry to "+t.endpoint)
	refused, err := a.px.CallRefusable(ctx, &change, "telemetry set", t.flags)
	if err == nil {
		err = refusal("these traces", refused, nil)
	}
	if err != nil {
		left := "telemetry is not set"
		if old != "" {
			left = "telemetry is now unset; it was toward " + old
		}
		say(ctx, a.u, left)
		return err
	}
	a.warn(ctx, slices.DeleteFunc(change.Findings, func(f proxy.Finding) bool { return f.Resource != "settings" }))
	return nil
}

// keptRecreate agrees, before anything changes, to the recreate a credential
// kept in .env may need: whether the container has read it is known only
// once telemetry is set. A run that wrote has agreed already.
func keptRecreate(ctx context.Context, a *adding, kept []string) error {
	if len(kept) == 0 || a.agreed {
		return nil
	}
	return a.consent(ctx, "recreate", "the container is recreated if it has not read "+strings.Join(kept, ", ")+
		" yet: requests in flight are given time to finish, and the rate-limit windows restart")
}

// testSpan sends one span toward endpoint and says how far it got.
func testSpan(ctx context.Context, a *adding, endpoint string) (string, error) {
	say(ctx, a.u, "telemetry test: one span to "+endpoint)
	rep, err := a.px.TelemetryTest(ctx)
	if err != nil {
		return "", err
	}
	say(ctx, a.u, rep.Line())
	if rep.Outcome != "accepted" {
		return "", errors.New("the test span was not accepted: telemetry stays set toward " + endpoint)
	}
	return "Done: traces go to " + endpoint + ".", nil
}

// traceOff turns traces off, once agreed; the variables of their
// credentials stay in .env (owner, C63-Q3).
func traceOff(ctx context.Context, a *adding) (string, error) {
	old, _, err := telemetryNow(ctx, a.px)
	switch {
	case err != nil:
		return "", err
	case old == "":
		return "Telemetry is not set; nothing was changed.", nil
	}
	if err := confirm(ctx, a.u, "which", "telemetry toward "+old+" is turned off: no span is sent, and the variables of its credentials stay in .env"); err != nil {
		return "", err
	}
	var change proxy.Change
	if err := a.px.Call(ctx, &change, "telemetry unset", []string{"--yes"}); err != nil {
		return "", err
	}
	return "Done: telemetry is off.", nil
}
