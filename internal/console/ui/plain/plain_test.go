package plain

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/console/ui"
)

func TestPlainDefault(t *testing.T) {
	got, err := New(&bytes.Buffer{}).Ask(context.Background(), ui.Question{ID: "name", Default: "main", HasDefault: true})
	if err != nil || got != "main" {
		t.Fatalf("got %q, %v; want main", got, err)
	}
}

func TestPlainMissingNamesFlag(t *testing.T) {
	_, err := New(&bytes.Buffer{}).Ask(context.Background(), ui.Question{ID: "name", Flag: "key-name"})
	var usage *ui.UsageError
	if !errors.As(err, &usage) || err.Error() != "missing --key-name" {
		t.Fatalf("err = %v, want missing --key-name", err)
	}
}

func TestPlainRevealOnceAlone(t *testing.T) {
	var out bytes.Buffer
	got, err := New(&out).Ask(context.Background(), ui.Question{ID: "key", Kind: ui.KindReveal, Prompt: "Store this key", Value: "sk-fake"})
	if err != nil || got != "" {
		t.Fatalf("got %q, %v", got, err)
	}
	if out.String() != "sk-fake\n" {
		t.Fatalf("stdout = %q, want the value alone", out.String())
	}
}

func TestPlainShowLines(t *testing.T) {
	var out bytes.Buffer
	f := New(&out)
	for _, line := range []string{"one", "two"} {
		if err := f.Show(context.Background(), line); err != nil {
			t.Fatal(err)
		}
	}
	if out.String() != "one\ntwo\n" {
		t.Fatalf("stdout = %q", out.String())
	}
}

func TestPlainMissingWithPrompt(t *testing.T) {
	_, err := New(&bytes.Buffer{}).Ask(context.Background(), ui.Question{ID: "yes", Flag: "yes", Prompt: "writing x"})
	var usage *ui.UsageError
	if !errors.As(err, &usage) || err.Error() != "missing --yes: writing x" {
		t.Fatalf("err = %v, want missing --yes: writing x", err)
	}
}

// TestAsks: the plain front never asks; the other half is in ui's tests.
func TestAsks(t *testing.T) {
	if ui.New(New(io.Discard), nil).Asks() {
		t.Fatal("the plain front asks")
	}
}

func TestPlainPaid(t *testing.T) {
	var out bytes.Buffer
	got, err := New(&out).Ask(context.Background(), ui.Question{ID: "check", Kind: ui.KindPaid, Prompt: "2 paid call(s) in all"})
	if err != nil || got != "check" {
		t.Fatalf("got %q, %v; want check", got, err)
	}
	if out.String() != "2 paid call(s) in all\n" {
		t.Fatalf("stdout = %q", out.String())
	}
}

func TestPlainWorkWaits(t *testing.T) {
	var out bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan error, 1)
	go func() {
		_, err := New(&out).Ask(ctx, ui.Question{ID: "up", Kind: ui.KindWork, Prompt: "restarting"})
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("returned before cancel: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-got:
		if !errors.Is(err, context.Canceled) || out.Len() != 0 {
			t.Fatalf("err = %v, stdout %q", err, out.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no return after cancel")
	}
}
