package transport

import (
	"bytes"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

const planted = "PLANTED-credential-value"

// verbs are the fmt verbs of design section 8.
var verbs = []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"}

func TestSecretKeepsTheValue(t *testing.T) {
	if s := NewSecret(planted); *s.value != planted {
		t.Errorf("value %q", *s.value)
	}
}

func TestSecretIsRedacted(t *testing.T) {
	s := NewSecret(planted)
	type inner struct {
		Credential Secret
		hidden     Secret // fmt calls no method on an unexported field
	}
	holder := struct {
		Name       string
		Credential Secret
		Pointer    *Secret
		Inner      *inner // %s, %x, %d and %q are wrong verbs for a pointer: fmt then prints it with methods off
	}{"p", s, &s, &inner{s, s}}
	for _, verb := range verbs {
		if out := fmt.Sprintf(verb, s); out != redacted {
			t.Errorf("%s: %s", verb, out)
		}
		for _, v := range []any{s, &s, holder, &holder, holder.Inner, *holder.Inner, []Secret{s}, map[string]Secret{"k": s}} {
			out := fmt.Sprintf(verb, v)
			if strings.Contains(out, planted) || strings.Contains(out, fmt.Sprintf("%x", planted)) || strings.Contains(out, fmt.Sprintf("%X", planted)) {
				t.Errorf("%s of %T: %s", verb, v, out)
			}
		}
	}

	for name, marshal := range map[string]func(any) ([]byte, error){"v1": json.Marshal, "v2": func(v any) ([]byte, error) { return jsonv2.Marshal(v) }} {
		out, err := marshal(holder)
		if err != nil || strings.Contains(string(out), planted) || !strings.Contains(string(out), `"Credential":"[redacted]"`) {
			t.Errorf("json %s: %s, error %v", name, out, err)
		}
	}

	var logs bytes.Buffer
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("x", "credential", s, "holder", holder)
	slog.New(slog.NewTextHandler(&logs, nil)).Info("x", "credential", s, "holder", holder)
	if strings.Contains(logs.String(), planted) || !strings.Contains(logs.String(), redacted) {
		t.Errorf("logs: %s", &logs)
	}
}

// TestSecretSetHeader: the value reaches only the header named, after its
// prefix; the zero Secret sets nothing.
func TestSecretSetHeader(t *testing.T) {
	h := http.Header{}
	Secret{}.SetHeader(h, "Authorization", "Bearer ")
	if len(h) != 0 {
		t.Errorf("the zero Secret set %v", h)
	}
	NewSecret(planted).SetHeader(h, "Authorization", "Bearer ")
	NewSecret(planted).SetHeader(h, "x-api-key", "")
	if len(h) != 2 || h.Get("Authorization") != "Bearer "+planted || h.Get("X-Api-Key") != planted {
		t.Errorf("header %v", h)
	}
}

func TestSecretRedact(t *testing.T) {
	if got := NewSecret(planted).Redact("bad key " + planted + ", " + planted); got != "bad key [redacted], [redacted]" {
		t.Errorf("Redact: %q", got)
	}
	if got := (Secret{}).Redact("text"); got != "text" {
		t.Errorf("zero Secret: %q", got)
	}
	if got := NewSecret("").Redact("text"); got != "text" {
		t.Errorf("empty Secret: %q", got)
	}
}
