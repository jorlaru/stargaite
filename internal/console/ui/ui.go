// Package ui is the console's question layer: a task asks typed questions
// through a UI, which takes each answer from the flags when one was given and
// otherwise from a front, the plain one without a terminal or the guided one
// at a terminal. It validates every answer the same way for both, and cleans
// every text a front prints (OPS-11). It imports no terminal library.
package ui

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// ErrLeave is Esc on a question: the reader leaves the task.
var ErrLeave = errors.New("left the task")

// ErrQuit is Ctrl-C: the reader quits the console.
var ErrQuit = errors.New("quit")

// maxAsks bounds the asks of one question whose answers fail its Check, since
// the plain front gives the same answer every time.
const maxAsks = 3

// UsageError is a wrong flag value or a missing flag: exit 2 (OPS-2).
type UsageError struct{ Msg string }

func (e *UsageError) Error() string { return e.Msg }

// Kind says which piece asks a question.
type Kind int

// The kinds of question.
const (
	KindPick Kind = iota
	KindText
	KindConfirm
	KindReveal
	KindPickMany
	KindSecret
	KindWork
)

// Option is one choice of a pick: Value is the answer, Label what is shown.
type Option struct{ Value, Label string }

// Step is one row of a task's step table.
type Step struct{ ID, Title string }

// Bar is the step bar of a question: the titles of the task's steps and the
// index of the current one, -1 outside a task.
type Bar struct {
	Titles  []string
	Current int
}

// Question is what a task asks. ID keys the answer given as a flag; Flag is
// that flag's name, without the dashes. A question asked once per item (a
// key per provider) names it in Item: its flag then takes item=value pairs.
type Question struct {
	ID, Step, Prompt, Flag string
	Kind                   Kind
	Options                []Option // pick, confirm, multi-pick
	Default                string
	HasDefault             bool
	Value                  string             // reveal: what is shown
	Check                  func(string) error // optional; never applied to a secret
	Item                   string             // the item a per-item flag answers
	Other                  string             // multi-pick: from the flags, an item that is no option is taken as typed
	Bar                    Bar                // set by UI before Front.Ask
}

// Front asks the reader. Show gets text already cleaned.
type Front interface {
	Ask(ctx context.Context, q Question) (string, error)
	Show(ctx context.Context, text string) error
}

// UI is the questions of one session. It is used by one goroutine.
type UI struct {
	front   Front
	answers map[string]string
	environ []string           // where a secret's flag finds its variable
	bare    map[string]bareUse // per ID: the first item a bare per-item value answered
	steps   []Step             // the running task's table; nil outside a task
	inTask  bool
}

// bareUse is the item, and the prompt it asked, that a per-item flag given
// without pairs answered first.
type bareUse struct{ item, prompt string }

// New returns a UI that takes answers from the flags first, then from front.
func New(front Front, answers map[string]string) *UI {
	return &UI{front: front, answers: answers, bare: map[string]bareUse{}}
}

// WithEnv sets the environment a secret's flag names its variable in, and
// returns u.
func (u *UI) WithEnv(environ []string) *UI {
	u.environ = environ
	return u
}

// Given reports whether the flags answer q.
func (u *UI) Given(q Question) bool {
	_, ok := u.answer(q)
	return ok
}

// Run runs a task with its step table and restores the previous table on
// return, so tasks nest.
func (u *UI) Run(ctx context.Context, steps []Step, fn func(context.Context) (string, error)) (string, error) {
	savedSteps, savedIn := u.steps, u.inTask
	u.steps, u.inTask = steps, true
	defer func() { u.steps, u.inTask = savedSteps, savedIn }()
	return fn(ctx)
}

// Pick asks for one of q.Options.
func (u *UI) Pick(ctx context.Context, q Question) (string, error) {
	q.Kind = KindPick
	return u.ask(ctx, q)
}

// Text asks for a line of text.
func (u *UI) Text(ctx context.Context, q Question) (string, error) {
	q.Kind = KindText
	return u.ask(ctx, q)
}

// Confirm asks yes or no; the answers are "yes" and "no".
func (u *UI) Confirm(ctx context.Context, q Question) (bool, error) {
	q.Kind = KindConfirm
	q.Options = []Option{{Value: "yes", Label: "Yes"}, {Value: "no", Label: "No"}}
	a, err := u.ask(ctx, q)
	return a == "yes", err
}

// PickMany asks for one or more of q.Options. From the flags, with q.Other
// set, an item that is no option is taken as typed.
func (u *UI) PickMany(ctx context.Context, q Question) ([]string, error) {
	q.Kind = KindPickMany
	a, err := u.ask(ctx, q)
	if err != nil {
		return nil, err
	}
	return strings.Split(a, ","), nil
}

// Secret asks for a value that is never shown, named by q.Prompt. From the
// flags, the answer is the name of a variable in the environment WithEnv
// gave. The value must be one .env can hold.
func (u *UI) Secret(ctx context.Context, q Question) (string, error) {
	q.Kind, q.Check = KindSecret, nil
	q, err := u.prepare(q)
	if err != nil {
		return "", err
	}
	name, given, err := u.fromFlags(q)
	var v string
	switch {
	case err != nil:
		return "", err
	case given:
		v, err = u.variable(q.Flag, name)
	default:
		v, err = u.front.Ask(ctx, q)
	}
	if err == nil {
		err = Storable(q.Prompt, v)
	}
	if err != nil {
		return "", err
	}
	return v, nil
}

// variable is the value of the environment variable name, which a flag gave.
// The error never holds what was given.
func (u *UI) variable(flag, name string) (string, error) {
	if regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`).MatchString(name) {
		for _, kv := range u.environ {
			if k, v, _ := strings.Cut(kv, "="); k == name {
				return v, nil
			}
		}
	}
	return "", &UsageError{Msg: "--" + flag + " takes the name of a variable exported to the console"}
}

// Work runs fn while the front shows q, and stops it when the reader leaves
// or quits there: fn's context then ends, and Work returns ErrLeave or
// ErrQuit once fn has returned. Otherwise it returns fn's error. fn may say
// lines: the ask runs on its own goroutine and touches no field of u.
func (u *UI) Work(ctx context.Context, q Question, fn func(context.Context) error) error {
	q.Kind = KindWork
	q, err := u.prepare(q)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	front, asked := u.front, make(chan error, 1)
	Go(func() {
		_, err := front.Ask(wctx, q)
		if pressed(err) {
			cancel()
		}
		asked <- err
	}, func(any) { cancel(); asked <- nil }) // fn then reports the ended context
	err = fn(wctx)
	cancel()
	if a := <-asked; pressed(a) {
		return a
	}
	return err
}

// pressed reports whether err is the reader leaving or quitting.
func pressed(err error) bool { return errors.Is(err, ErrLeave) || errors.Is(err, ErrQuit) }

// Reveal shows q.Value until the reader confirms storing it. It never takes
// an answer from the flags: showing is the point.
func (u *UI) Reveal(ctx context.Context, q Question) error {
	q.Kind = KindReveal
	q, err := u.prepare(q)
	if err != nil {
		return err
	}
	_, err = u.front.Ask(ctx, q)
	return err
}

// Say prints one line of untrusted text.
func (u *UI) Say(ctx context.Context, line string) error {
	return u.front.Show(ctx, OneLine(line))
}

// Show prints untrusted text of several lines.
func (u *UI) Show(ctx context.Context, text string) error {
	return u.front.Show(ctx, MultiLine(text))
}

// prepare checks q's step and returns the copy a front gets: prompt and
// labels cleaned, the step bar set. Values and Default stay, as answers.
func (u *UI) prepare(q Question) (Question, error) {
	bar := Bar{Current: -1}
	if u.inTask {
		bar.Current = slices.IndexFunc(u.steps, func(s Step) bool { return s.ID == q.Step })
		for _, s := range u.steps {
			bar.Titles = append(bar.Titles, s.Title)
		}
	}
	if (u.inTask && bar.Current < 0) || (!u.inTask && q.Step != "") {
		return q, fmt.Errorf("ui: question %s: step %q is not in the task's table", q.ID, q.Step)
	}
	q.Bar = bar
	q.Prompt = OneLine(q.Prompt)
	q.Options = slices.Clone(q.Options)
	for i := range q.Options {
		q.Options[i].Label = OneLine(q.Options[i].Label)
	}
	return q, nil
}

// ask is the answer chain: flags or front, the default, the options, the
// check, and the re-ask after a failed check at the front.
func (u *UI) ask(ctx context.Context, q Question) (string, error) {
	q, err := u.prepare(q)
	if err != nil {
		return "", err
	}
	a, given, err := u.fromFlags(q)
	if err != nil {
		return "", err
	}
	if given {
		a = orDefault(q, a)
		err := isOption(q, a, true)
		if err == nil && q.Check != nil {
			err = q.Check(a)
		}
		if err != nil {
			return "", &UsageError{Msg: "--" + q.Flag + ": " + err.Error()}
		}
		return a, nil
	}
	for asks := 1; ; asks++ {
		a, err := u.front.Ask(ctx, q)
		if err != nil {
			return "", err
		}
		a = orDefault(q, a)
		if err := isOption(q, a, false); err != nil {
			return "", fmt.Errorf("ui: question %s: %w", q.ID, err)
		}
		if q.Check == nil {
			return a, nil
		}
		err = q.Check(a)
		if err == nil {
			return a, nil
		}
		if asks == maxAsks {
			return "", err
		}
		if err := u.Say(ctx, err.Error()); err != nil {
			return "", err
		}
	}
}

// answer is the flags' answer to q. For a per-item question, a value that
// holds "=" is item=value pairs, and only q.Item's pair answers it.
func (u *UI) answer(q Question) (string, bool) {
	a, ok := u.answers[q.ID]
	if !ok || q.Item == "" || !strings.Contains(a, "=") {
		return a, ok
	}
	for _, pair := range strings.Split(a, ",") {
		if item, v, _ := strings.Cut(pair, "="); item == q.Item {
			return v, true
		}
	}
	return "", false
}

// fromFlags is answer, with the rule for a per-item flag given without
// pairs: it answers every item that asks what the first one asked, and a
// second item asking something else is a usage error, so one item's value
// never answers another's question.
func (u *UI) fromFlags(q Question) (string, bool, error) {
	a, ok := u.answer(q)
	if !ok || q.Item == "" || strings.Contains(u.answers[q.ID], "=") {
		return a, ok, nil
	}
	first, seen := u.bare[q.ID]
	switch {
	case !seen:
		u.bare[q.ID] = bareUse{item: q.Item, prompt: q.Prompt}
	case first.item != q.Item && first.prompt != q.Prompt:
		return "", false, &UsageError{Msg: fmt.Sprintf("--%s: several providers need a key each: give %s=VAR,%s=VAR",
			q.Flag, first.item, q.Item)}
	}
	return a, true, nil
}

// orDefault is a, or q's default when a is empty and q has one.
func orDefault(q Question, a string) string {
	if a == "" && q.HasDefault {
		return q.Default
	}
	return a
}

// isOption refuses a pick or confirm answer that is not one of q.Options,
// and a multi-pick answer, items joined by ",", with an item that is not.
// From the flags, a multi-pick with q.Other takes any non-empty item.
func isOption(q Question, a string, fromFlags bool) error {
	items := []string{a}
	switch {
	case q.Kind == KindPickMany:
		items = strings.Split(a, ",")
	case q.Kind != KindPick && q.Kind != KindConfirm:
		return nil
	}
	values := make([]string, len(q.Options))
	for i, o := range q.Options {
		values[i] = o.Value
	}
	for _, it := range items {
		if !slices.Contains(values, it) && (!fromFlags || q.Other == "" || it == "") {
			return fmt.Errorf("%s is not one of %s", it, strings.Join(values, ", "))
		}
	}
	return nil
}
