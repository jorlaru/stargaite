package transport

import (
	"bytes"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"fmt"
	"log/slog"
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
