package task

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jorlaru/stargaite/internal/console/host"
	"github.com/jorlaru/stargaite/internal/console/paid"
	"github.com/jorlaru/stargaite/internal/console/proxy"
	"github.com/jorlaru/stargaite/internal/console/ui"
)

// The steps Give an app or agent its key and Change a key share: the keys
// read, the aliases a key may call, and the hand-over of a new secret.

// listKeys reads the client keys.
func (r *run) listKeys(ctx context.Context) error {
	var reply proxy.KeysReply
	if err := r.px.Call(ctx, &reply, "keys list", nil); err != nil {
		return err
	}
	r.keys = reply.Keys
	return nil
}

// key is the listed key named n.
func (r *run) key(n string) (proxy.Key, bool) {
	return find(r.keys, func(k proxy.Key) bool { return k.Name == n })
}

// noAlias is the error of an alias a key would call that does not exist.
func noAlias(n string) error {
	return fmt.Errorf("alias %s does not exist: ./setup.sh alias makes it", n)
}

// pickAliases is the aliases a key may call: picked from those listed, in
// their order, or from the flag in its order, or "main" without a terminal
// or the flag. Each must be listed. q carries the ID, flag, step and prompt.
func (r *run) pickAliases(ctx context.Context, q ui.Question) ([]string, error) {
	aliases := []string{"main"}
	if r.u.Given(q) || r.u.Asks() {
		q.Other = "unlisted" // from the flags, a name no alias has reaches the check below
		for _, m := range r.models {
			q.Options = append(q.Options, ui.Option{Value: m.Name, Label: m.Name + " → " + strings.Join(m.Targets, ", ")})
		}
		var err error
		if aliases, err = r.u.PickMany(ctx, q); err != nil {
			return nil, err
		}
	}
	for _, a := range aliases {
		if _, ok := r.model(a); !ok {
			return nil, noAlias(a)
		}
	}
	return aliases, nil
}

// tryAlias is the alias a try goes through, the first of aliases that is
// listed ("*": the first alias listed), and its target count; ok is false
// when none is listed.
func (r *run) tryAlias(aliases []string) (alias string, n int, ok bool) {
	for _, a := range aliases {
		if a == "*" && len(r.models) > 0 {
			a = r.models[0].Name
		}
		if m, listed := r.model(a); listed {
			return m.Name, len(m.Targets), true
		}
	}
	return "", 0, false
}

// newSecret runs keys create or keys rotate (words) with flags for key name,
// how being "made" or "rotated", and returns its new secret. What a stop
// before the secret is stored leaves is said first, so it stays in the
// terminal's history however the run ends: Ctrl-C or a signal here ends the
// program before an error is said, and the reveal can end the same way.
func (r *run) newSecret(ctx context.Context, words string, flags []string, name, how string) (string, error) {
	say(ctx, r.u, "if this stops before you store its secret, key "+name+" may be "+how+" without it: Change a key rotates it")
	secret, findings, err := r.px.NewSecret(ctx, words, flags, name)
	if err != nil {
		return "", err
	}
	r.made = append(r.made, "key "+name+" "+how)
	r.warn(ctx, findings, false, "keys/"+name)
	return secret, nil
}

// handOver shows key name's new secret once, then tries it through the
// paid step and says how a client calls it; how is "made" or "rotated".
// tried is false after Not now, or when no alias it may call exists.
func (r *run) handOver(ctx context.Context, name, how, secret string, aliases []string) (tried bool, err error) {
	// newSecret's line, said before the key was made or rotated, already says
	// how a secret lost before it is stored is recovered.
	say(ctx, r.u, "key "+name+" is "+how+": its secret is shown next, once")
	if err := r.u.Reveal(ctx, ui.Question{ID: "secret", Step: "key", Prompt: "client key " + name + ", shown once and written nowhere: store it now",
		Value: secret}); err != nil {
		return false, err
	}
	alias, n, ok := r.tryAlias(aliases)
	if !ok {
		say(ctx, r.u, "no alias key "+name+" may call exists, so it is not tried")
		return false, nil
	}
	say(ctx, r.u, fmt.Sprintf("try alias %s: up to %d paid call(s), one per target if the earlier fail, each capped at 256 output tokens, never retried", alias, n))
	say(ctx, r.u, "Esc or Ctrl-C stops the try at the server: no further target is called, and a call already sent may still be billed")
	b, err := r.u.Paid(ctx, ui.Question{ID: "try", Step: "try", Prompt: fmt.Sprintf("try it: up to %d paid call(s)", n),
		Options: []ui.Option{{Label: "Try it"}, {Label: "Not now"}}}, n)
	if err != nil {
		return false, err
	}
	if b != nil {
		if err := r.try(ctx, b, n, secret, alias); err != nil {
			return false, err
		}
	}
	return b != nil, r.example(ctx, alias)
}

// try sends the one test call of b's n calls with secret through alias,
// under a work piece that Esc or Ctrl-C stops at the server, and says its
// answer. A call that did not complete says the container's last log lines.
func (r *run) try(ctx context.Context, b *paid.Budget, n int, secret, alias string) error {
	var got host.Completion
	err := r.u.Work(ctx, ui.Question{ID: "trying", Step: "try", Prompt: "trying alias " + alias}, func(ctx context.Context) error {
		var err error
		got, err = r.h.TryCompletion(ctx, b, n, secret, alias)
		return err
	})
	stopped := "the try through alias " + alias + " was stopped: the call in flight may still be billed"
	switch {
	case errors.Is(err, ui.ErrLeave), errors.Is(err, ui.ErrQuit):
		say(ctx, r.u, stopped)
		return err
	case err != nil && ctx.Err() != nil: // the root ended: a signal
		say(ctx, r.u, stopped)
		return stoppedTry{stopped, ctx.Err()}
	case err != nil:
		say(ctx, r.u, "the last 30 log lines:")
		r.h.Logs(ctx, sayer(ctx, r.u))
		return err
	case got.Status/100 != 2 && got.Message != "":
		return fmt.Errorf("the test call answered %d: %s", got.Status, got.Message)
	case got.Status/100 != 2:
		return fmt.Errorf("the test call answered %d", got.Status)
	}
	say(ctx, r.u, "answer: "+got.Content)
	say(ctx, r.u, fmt.Sprintf("finish_reason: %s, %.3f s, from %s", cmp.Or(got.FinishReason, "-"), got.Took.Seconds(), cmp.Or(got.Target, "-")))
	return nil
}

// stoppedTry is a try stopped by the root's end: its words for the reader,
// and the context's error it unwraps to.
type stoppedTry struct {
	msg   string
	cause error
}

func (s stoppedTry) Error() string { return s.msg }
func (s stoppedTry) Unwrap() error { return s.cause }

// example says the base URL, the alias and a call a client makes with the
// key, which reads it from $STARGAITE_KEY, so the example never holds it.
func (r *run) example(ctx context.Context, alias string) error {
	p, err := r.h.Plane()
	if err != nil {
		return err
	}
	ca := ""
	if p.CA != "" {
		ca = "--cacert tls/" + p.CA + " "
	}
	say(ctx, r.u, "Point an OpenAI-compatible client at base URL "+p.BaseURL+", with model "+alias+" and this key, such as:")
	say(ctx, r.u, "  curl "+ca+p.BaseURL+`/chat/completions -H "Authorization: Bearer $STARGAITE_KEY" -H "Content-Type: application/json" -d '{"model":"`+
		alias+`","max_tokens":256,"messages":[{"role":"user","content":"Hello"}]}'`)
	return nil
}
