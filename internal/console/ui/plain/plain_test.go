package plain

import (
	"bytes"
	"context"
	"errors"
	"testing"

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
