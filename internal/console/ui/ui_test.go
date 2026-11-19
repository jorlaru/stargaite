package ui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeFront answers from a list, records every question and every Show.
type fakeFront struct {
	t       *testing.T
	answers []string
	asked   []Question
	shown   []string
}

func (f *fakeFront) Ask(_ context.Context, q Question) (string, error) {
	f.asked = append(f.asked, q)
	if len(f.answers) == 0 {
		f.t.Fatalf("question %s asked with no answer left", q.ID)
	}
	a := f.answers[0]
	if len(f.answers) > 1 {
		f.answers = f.answers[1:]
	}
	return a, nil
}

func (f *fakeFront) Show(_ context.Context, text string) error {
	f.shown = append(f.shown, text)
	return nil
}

// refusingFront fails the test if it is asked anything.
type refusingFront struct{ t *testing.T }

func (f refusingFront) Ask(_ context.Context, q Question) (string, error) {
	f.t.Fatalf("question %s reached the front", q.ID)
	return "", nil
}

func (f refusingFront) Show(context.Context, string) error { return nil }

func aliasQuestion() Question {
	return Question{ID: "alias", Prompt: "Which alias?", Flag: "alias",
		Options: []Option{{Value: "main", Label: "main"}, {Value: "fast", Label: "fast"}}}
}

func TestAnswerFromFlagsSkipsFront(t *testing.T) {
	u := New(refusingFront{t}, map[string]string{"name": "x"})
	got, err := u.Text(context.Background(), Question{ID: "name", Flag: "name"})
	if err != nil || got != "x" {
		t.Fatalf("got %q, %v; want x", got, err)
	}
}

func TestDefaultOnEmpty(t *testing.T) {
	u := New(&fakeFront{t: t, answers: []string{""}}, nil)
	got, err := u.Text(context.Background(), Question{ID: "name", Default: "main", HasDefault: true})
	if err != nil || got != "main" {
		t.Fatalf("got %q, %v; want main", got, err)
	}
}

func TestPickRefusesUnknownFlagValue(t *testing.T) {
	u := New(refusingFront{t}, map[string]string{"alias": "other"})
	_, err := u.Pick(context.Background(), aliasQuestion())
	var usage *UsageError
	if !errors.As(err, &usage) {
		t.Fatalf("err = %v, want a *UsageError", err)
	}
	if want := "--alias: other is not one of main, fast"; err.Error() != want {
		t.Fatalf("err = %q, want %q", err, want)
	}
}

func TestPickFrontAnswerChecked(t *testing.T) {
	u := New(&fakeFront{t: t, answers: []string{"fast"}}, nil)
	got, err := u.Pick(context.Background(), aliasQuestion())
	if err != nil || got != "fast" {
		t.Fatalf("got %q, %v; want fast", got, err)
	}
	u = New(&fakeFront{t: t, answers: []string{"other"}}, nil)
	if _, err := u.Pick(context.Background(), aliasQuestion()); err == nil {
		t.Fatal("a front answer outside the options was taken")
	}
}

var errBad = errors.New("bad is refused")

func refuseBad(s string) error {
	if s == "bad" {
		return errBad
	}
	return nil
}

func TestCheckReasksAtFront(t *testing.T) {
	front := &fakeFront{t: t, answers: []string{"bad", "good"}}
	u := New(front, nil)
	got, err := u.Text(context.Background(), Question{ID: "name", Check: refuseBad})
	if err != nil || got != "good" {
		t.Fatalf("got %q, %v; want good", got, err)
	}
	if len(front.shown) != 1 || front.shown[0] != errBad.Error() {
		t.Fatalf("shown = %q, want the check's message once", front.shown)
	}
}

func TestCheckFlagsIsUsage(t *testing.T) {
	u := New(refusingFront{t}, map[string]string{"name": "bad"})
	_, err := u.Text(context.Background(), Question{ID: "name", Flag: "key-name", Check: refuseBad})
	var usage *UsageError
	if !errors.As(err, &usage) || err.Error() != "--key-name: bad is refused" {
		t.Fatalf("err = %v, want the usage error naming the flag", err)
	}
}

func TestCheckReaskIsCapped(t *testing.T) {
	front := &fakeFront{t: t, answers: []string{"bad"}}
	u := New(front, nil)
	_, err := u.Text(context.Background(), Question{ID: "name", Check: refuseBad})
	if !errors.Is(err, errBad) {
		t.Fatalf("err = %v, want the check's error", err)
	}
	if len(front.asked) != maxAsks || maxAsks != 3 {
		t.Fatalf("asked %d times, want 3", len(front.asked))
	}
}

func TestAskCleansQuestion(t *testing.T) {
	front := &fakeFront{t: t, answers: []string{"v\x1b[2J"}}
	u := New(front, nil)
	q := Question{ID: "alias", Prompt: "pick\x1b[2J", Default: "d\x1b[2J", HasDefault: true,
		Options: []Option{{Value: "v\x1b[2J", Label: "label\x1b[2J"}}}
	if _, err := u.Pick(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	got := front.asked[0]
	if got.Prompt != "pick[2J" || got.Options[0].Label != "label[2J" {
		t.Fatalf("prompt %q, label %q: not cleaned", got.Prompt, got.Options[0].Label)
	}
	if got.Options[0].Value != "v\x1b[2J" || got.Default != "d\x1b[2J" {
		t.Fatalf("value %q, default %q: changed", got.Options[0].Value, got.Default)
	}
	if q.Prompt != "pick\x1b[2J" || q.Options[0].Label != "label\x1b[2J" {
		t.Fatal("the caller's question was changed")
	}
}

func TestStepOutsideTable(t *testing.T) {
	u := New(refusingFront{t}, nil)
	_, err := u.Text(context.Background(), Question{ID: "name", Step: "model"})
	if err == nil || err.Error() != `ui: question name: step "model" is not in the task's table` {
		t.Fatalf("err = %v", err)
	}
	_, err = u.Run(context.Background(), []Step{{ID: "show", Title: "what exists"}}, func(ctx context.Context) (string, error) {
		return u.Text(ctx, Question{ID: "name"})
	})
	if err == nil || err.Error() != `ui: question name: step "" is not in the task's table` {
		t.Fatalf("err = %v", err)
	}
}

func TestRunNestsSteps(t *testing.T) {
	front := &fakeFront{t: t, answers: []string{"x"}}
	u := New(front, nil)
	outer := []Step{{"name", "name"}, {"model", "model"}}
	inner := []Step{{"check", "check"}}
	got, err := u.Run(context.Background(), outer, func(ctx context.Context) (string, error) {
		if _, err := u.Run(ctx, inner, func(ctx context.Context) (string, error) {
			return u.Text(ctx, Question{ID: "a", Step: "check"})
		}); err != nil {
			return "", err
		}
		return u.Text(ctx, Question{ID: "b", Step: "model"})
	})
	if err != nil || got != "x" {
		t.Fatalf("got %q, %v", got, err)
	}
	if b := front.asked[0].Bar; len(b.Titles) != 1 || b.Current != 0 {
		t.Fatalf("inner bar = %+v", b)
	}
	if b := front.asked[1].Bar; len(b.Titles) != 2 || b.Titles[1] != "model" || b.Current != 1 {
		t.Fatalf("outer bar = %+v", b)
	}
	if _, err := u.Text(context.Background(), Question{ID: "c"}); err != nil {
		t.Fatal(err)
	}
	if b := front.asked[2].Bar; b.Current != -1 || len(b.Titles) != 0 {
		t.Fatalf("bar outside a task = %+v", b)
	}
}

func TestConfirmMapsYesNo(t *testing.T) {
	for _, tc := range []struct {
		answer string
		want   bool
	}{{"yes", true}, {"no", false}} {
		front := &fakeFront{t: t, answers: []string{tc.answer}}
		got, err := New(front, nil).Confirm(context.Background(), Question{ID: "sure"})
		if err != nil || got != tc.want {
			t.Fatalf("%s: got %v, %v", tc.answer, got, err)
		}
		if front.asked[0].Kind != KindConfirm {
			t.Fatalf("kind = %v", front.asked[0].Kind)
		}
	}
	_, err := New(refusingFront{t}, map[string]string{"sure": "maybe"}).Confirm(context.Background(), Question{ID: "sure", Flag: "yes"})
	if err == nil || err.Error() != "--yes: maybe is not one of yes, no" {
		t.Fatalf("err = %v", err)
	}
}

func TestRevealGoesToFront(t *testing.T) {
	front := &fakeFront{t: t, answers: []string{""}}
	if err := New(front, map[string]string{"key": "ignored"}).Reveal(context.Background(), Question{ID: "key", Value: "sk-fake"}); err != nil {
		t.Fatal(err)
	}
	if len(front.asked) != 1 || front.asked[0].Kind != KindReveal || front.asked[0].Value != "sk-fake" {
		t.Fatalf("asked = %+v", front.asked)
	}
}

func TestSayAndShowClean(t *testing.T) {
	front := &fakeFront{t: t}
	u := New(front, nil)
	if err := u.Say(context.Background(), "a\nb\x1b[2J"); err != nil {
		t.Fatal(err)
	}
	if err := u.Show(context.Background(), "a\nb\x1b[2J"); err != nil {
		t.Fatal(err)
	}
	if front.shown[0] != "a b[2J" || front.shown[1] != "a\nb[2J" {
		t.Fatalf("shown = %q", front.shown)
	}
}

func TestFrontErrorReturned(t *testing.T) {
	u := New(errFront{}, nil)
	if _, err := u.Pick(context.Background(), aliasQuestion()); !errors.Is(err, ErrLeave) {
		t.Fatalf("err = %v, want ErrLeave", err)
	}
}

type errFront struct{}

func (errFront) Ask(context.Context, Question) (string, error) { return "", ErrLeave }
func (errFront) Show(context.Context, string) error            { return nil }

func TestGoRecovers(t *testing.T) {
	got := make(chan any, 1)
	Go(func() { panic("x") }, func(v any) { got <- v })
	if v := <-got; v != "x" {
		t.Fatalf("onPanic got %v", v)
	}
}

func TestStorable(t *testing.T) {
	const cannot = "ACME_API_KEY holds ' or a newline, or ends in \\: .env cannot hold it, so nothing was written"
	for _, tc := range []struct{ v, want string }{
		{"", "ACME_API_KEY is empty"},
		{"it's", cannot},
		{"two\nlines", cannot},
		{"ends\\", cannot},
		{"$ #\\", cannot},
		{"s3cr3t $HOME #x y\\z", ""},
		{"$ #\\x", ""},
	} {
		err := Storable("ACME_API_KEY", tc.v)
		if (err == nil) != (tc.want == "") || (err != nil && err.Error() != tc.want) {
			t.Fatalf("Storable(%q) = %v, want %q", tc.v, err, tc.want)
		}
	}
}

func keyEnvQuestion(item, prompt string) Question {
	return Question{ID: "key-env", Flag: "key-env", Item: item, Prompt: prompt}
}

func TestItemPairs(t *testing.T) {
	ctx := context.Background()
	u := New(refusingFront{t}, map[string]string{"key-env": "openai=A,anthropic=B"})
	if got, err := u.Text(ctx, keyEnvQuestion("anthropic", "V")); err != nil || got != "B" {
		t.Fatalf("anthropic: got %q, %v; want B", got, err)
	}
	front := &fakeFront{t: t, answers: []string{"asked"}}
	u = New(front, map[string]string{"key-env": "openai=A,anthropic=B"})
	if got, err := u.Text(ctx, keyEnvQuestion("gemini", "V")); err != nil || got != "asked" || len(front.asked) != 1 {
		t.Fatalf("gemini: got %q, %v, asked %d; want the front asked", got, err, len(front.asked))
	}
	if u.Given(keyEnvQuestion("gemini", "V")) || !u.Given(keyEnvQuestion("openai", "V")) {
		t.Fatal("Given does not follow the item's pair")
	}

	u = New(refusingFront{t}, map[string]string{"key-env": "A"})
	for _, item := range []string{"x", "y"} {
		if got, err := u.Text(ctx, keyEnvQuestion(item, "V")); err != nil || got != "A" {
			t.Fatalf("bare value, item %s asking V: got %q, %v; want A", item, got, err)
		}
	}

	u = New(refusingFront{t}, map[string]string{"key-env": "A"}).WithEnv([]string{"A=s3cr3t"})
	if got, err := u.Secret(ctx, keyEnvQuestion("x", "V")); err != nil || got != "s3cr3t" {
		t.Fatalf("x asking V: got %q, %v", got, err)
	}
	if got, err := u.Secret(ctx, keyEnvQuestion("x", "V")); err != nil || got != "s3cr3t" {
		t.Fatalf("x asking V again: got %q, %v", got, err)
	}
	got, err := u.Secret(ctx, keyEnvQuestion("y", "W"))
	var usage *UsageError
	if !errors.As(err, &usage) || err.Error() != "--key-env: several providers need a key each: give x=VAR,y=VAR" || got != "" {
		t.Fatalf("y asking W: got %q, %v; want the usage error and no value", got, err)
	}
}

func TestGiven(t *testing.T) {
	u := New(refusingFront{t}, map[string]string{"yes": "yes"})
	if !u.Given(Question{ID: "yes"}) || u.Given(Question{ID: "replace"}) {
		t.Fatal("Given is wrong")
	}
}

func providerQuestion(other string) Question {
	return Question{ID: "provider", Flag: "provider", Prompt: "Which providers?", Other: other,
		Options: []Option{{Value: "a", Label: "a"}, {Value: "b", Label: "b"}, {Value: "+another", Label: "another"}}}
}

func TestPickManyFlags(t *testing.T) {
	ctx := context.Background()
	got, err := New(refusingFront{t}, map[string]string{"provider": "a,b"}).PickMany(ctx, providerQuestion(""))
	if err != nil || !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("got %q, %v; want [a b]", got, err)
	}
	_, err = New(refusingFront{t}, map[string]string{"provider": "a,zz"}).PickMany(ctx, providerQuestion(""))
	var usage *UsageError
	if !errors.As(err, &usage) || err.Error() != "--provider: zz is not one of a, b, +another" {
		t.Fatalf("err = %v", err)
	}
	got, err = New(refusingFront{t}, map[string]string{"provider": "zz"}).PickMany(ctx, providerQuestion("+another"))
	if err != nil || !slices.Equal(got, []string{"zz"}) {
		t.Fatalf("with Other: got %q, %v; want [zz]", got, err)
	}
	_, err = New(refusingFront{t}, map[string]string{"provider": "a,"}).PickMany(ctx, providerQuestion("+another"))
	if !errors.As(err, &usage) {
		t.Fatalf("an empty item: err = %v, want a usage error", err)
	}
}

func TestPickManyFront(t *testing.T) {
	ctx := context.Background()
	front := &fakeFront{t: t, answers: []string{"b,a"}}
	got, err := New(front, nil).PickMany(ctx, providerQuestion("+another"))
	if err != nil || !slices.Equal(got, []string{"b", "a"}) || front.asked[0].Kind != KindPickMany {
		t.Fatalf("got %q, %v; want [b a] from a multi-pick", got, err)
	}
	_, err = New(&fakeFront{t: t, answers: []string{"x"}}, nil).PickMany(ctx, providerQuestion("+another"))
	var usage *UsageError
	if err == nil || errors.As(err, &usage) || !strings.HasPrefix(err.Error(), "ui: question provider: ") {
		t.Fatalf("err = %v, want an internal error", err)
	}
}

func TestSecretFromEnv(t *testing.T) {
	const v = "s3cr3t $HOME #x y\\z"
	u := New(refusingFront{t}, map[string]string{"key-env": "SRC"}).WithEnv([]string{"OTHER=1", "SRC=" + v})
	got, err := u.Secret(context.Background(), Question{ID: "key-env", Flag: "key-env", Prompt: "ACME_API_KEY",
		Check: func(string) error { return errBad }})
	if err != nil || got != v {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestSecretFromFront(t *testing.T) {
	front := &fakeFront{t: t, answers: []string{"typed"}}
	got, err := New(front, nil).Secret(context.Background(), Question{ID: "key-env", Flag: "key-env", Prompt: "ACME_API_KEY"})
	if err != nil || got != "typed" || front.asked[0].Kind != KindSecret {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestSecretFlagNamesNoVariable(t *testing.T) {
	for _, given := range []string{"gsk_PastedValue123", "sk-pasted-value"} {
		u := New(refusingFront{t}, map[string]string{"key-env": given}).WithEnv([]string{"SRC=x"})
		got, err := u.Secret(context.Background(), Question{ID: "key-env", Flag: "key-env", Prompt: "ACME_API_KEY"})
		var usage *UsageError
		if !errors.As(err, &usage) || err.Error() != "--key-env takes the name of a variable exported to the console" || got != "" {
			t.Fatalf("%s: got %q, %v", given, got, err)
		}
		if strings.Contains(err.Error(), given) {
			t.Fatalf("the error holds what was given: %v", err)
		}
	}
}

func TestSecretRefusedValue(t *testing.T) {
	u := New(refusingFront{t}, map[string]string{"key-env": "SRC"}).WithEnv([]string{"SRC=it's"})
	got, err := u.Secret(context.Background(), Question{ID: "key-env", Flag: "key-env", Prompt: "ACME_API_KEY"})
	var usage *UsageError
	if err == nil || errors.As(err, &usage) || !strings.Contains(err.Error(), ".env cannot hold it") || got != "" {
		t.Fatalf("got %q, %v; want a plain error", got, err)
	}
	if strings.Contains(err.Error(), "it's") {
		t.Fatalf("the error holds the value: %v", err)
	}
}

// workFront answers a work question with err once its context ends, or at
// once when now is set, and records the kind and whether its context ended.
type workFront struct {
	err   error
	now   bool
	kind  chan Kind
	ended chan struct{}
}

func (f *workFront) Ask(ctx context.Context, q Question) (string, error) {
	f.kind <- q.Kind
	if f.now {
		return "", f.err
	}
	<-ctx.Done()
	close(f.ended)
	return "", ctx.Err()
}

func (f *workFront) Show(context.Context, string) error { return nil }

func TestWorkReturnsFnError(t *testing.T) {
	front := &workFront{kind: make(chan Kind, 1), ended: make(chan struct{})}
	err := New(front, nil).Work(context.Background(), Question{ID: "up", Prompt: "restarting"}, func(context.Context) error {
		if k := <-front.kind; k != KindWork {
			return fmt.Errorf("kind %v", k)
		}
		return errBad
	})
	if !errors.Is(err, errBad) {
		t.Fatalf("err = %v, want errBad", err)
	}
	select {
	case <-front.ended:
	default:
		t.Fatal("the front's context did not end before Work returned")
	}
}

func TestWorkLeave(t *testing.T) {
	front := &workFront{err: ErrLeave, now: true, kind: make(chan Kind, 1)}
	start := time.Now()
	err := New(front, nil).Work(context.Background(), Question{ID: "up", Prompt: "restarting"}, func(ctx context.Context) error {
		select { // bounded, so a Work that never cancels fails instead of hanging
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
			return errBad
		}
	})
	if !errors.Is(err, ErrLeave) || time.Since(start) > time.Second {
		t.Fatalf("err = %v after %s, want ErrLeave within 1s", err, time.Since(start))
	}
}
