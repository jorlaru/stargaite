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
// that flag's name, without the dashes.
type Question struct {
	ID, Step, Prompt, Flag string
	Kind                   Kind
	Options                []Option // pick, confirm
	Default                string
	HasDefault             bool
	Value                  string             // reveal: what is shown
	Check                  func(string) error // optional
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
	steps   []Step // the running task's table; nil outside a task
	inTask  bool
}

// New returns a UI that takes answers from the flags first, then from front.
func New(front Front, answers map[string]string) *UI {
	return &UI{front: front, answers: answers}
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
	if a, ok := u.answers[q.ID]; ok {
		a = orDefault(q, a)
		err := isOption(q, a)
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
		if err := isOption(q, a); err != nil {
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

// orDefault is a, or q's default when a is empty and q has one.
func orDefault(q Question, a string) string {
	if a == "" && q.HasDefault {
		return q.Default
	}
	return a
}

// isOption refuses a pick or confirm answer that is not one of q.Options.
func isOption(q Question, a string) error {
	if q.Kind != KindPick && q.Kind != KindConfirm {
		return nil
	}
	values := make([]string, len(q.Options))
	for i, o := range q.Options {
		values[i] = o.Value
	}
	if slices.Contains(values, a) {
		return nil
	}
	return fmt.Errorf("%s is not one of %s", a, strings.Join(values, ", "))
}
