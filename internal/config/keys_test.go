package config

import (
	"crypto/rand"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/auth"
	"github.com/jorlaru/stargaite/internal/domain"
)

// TestAuthenticate: the shape, an unknown public id, a wrong secret, a grace
// that ends at its valid_until on the injected clock, a third rotation during
// a grace and a revocation (design section 7, SEC-8).
func TestAuthenticate(t *testing.T) {
	s, st := newService(t)
	at := func(ms int64) time.Time { return time.UnixMilli(testNow + ms) }
	var first string
	commit(t, s, Change{Summary: "seed", Kind: "configuration", Mutate: func(st *State) (map[string]string, error) {
		seed(st)
		st.Doc.Keys["agent"] = domain.Key{Models: []string{"fast"}}
		var err error
		first, err = st.NewSecret("agent", testNow)
		return nil, err
	}})
	ok := func(token string, ms int64) bool {
		k, ok := s.Snapshot().Authenticate(token, at(ms))
		if ok != (k != nil) || (ok && k.Name != "agent") {
			t.Fatalf("Authenticate gave key %+v, ok %v", k, ok)
		}
		return ok
	}
	if !ok(first, 0) {
		t.Fatal("a new token is refused")
	}
	other, _, _, err := auth.Issue(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		"", first[:len(first)-1], first + "a", strings.ToUpper(first), strings.Replace(first, "sk-stargaite-", "sk-stargaitf-", 1),
		first[:26] + other[26:], // the right public id with another secret
		other,                   // an unknown public id
		"test-admin-key-of-at-least-32-characters", "Bearer " + first,
	} {
		if ok(bad, 0) {
			t.Errorf("token %q is accepted", bad)
		}
	}

	rotate := func(ms, grace int64) string {
		t.Helper()
		var token string
		commit(t, s, Change{Summary: "keys rotate agent", Kind: KindKey, Name: "agent", Mutate: func(st *State) (map[string]string, error) {
			var err error
			token, err = st.Rotate("agent", testNow+ms, testNow+ms+grace)
			return nil, err
		}})
		return token
	}
	secrets := func() int { return len(rows(t, st, "SELECT public_id FROM key_secrets WHERE key_name = 'agent'")) }

	second := rotate(0, 1000)
	if !ok(first, 999) || ok(first, 1000) || !ok(second, 0) || !ok(second, 1_000_000) || secrets() != 2 {
		t.Errorf("after one rotation: first at 999 %v, at 1000 %v; %d secrets", ok(first, 999), ok(first, 1000), secrets())
	}
	// A rotation during the grace invalidates the oldest secret at once.
	third := rotate(500, 1000)
	if ok(first, 500) || !ok(second, 1499) || ok(second, 1500) || !ok(third, 0) || secrets() != 2 {
		t.Errorf("after a rotation during the grace: %d secrets", secrets())
	}
	// An expired secret is deleted by the next rotation, and a grace of 0
	// ends the secret in use at once.
	fourth := rotate(5000, 0)
	if ok(third, 5000) || !ok(fourth, 5000) || ok(second, 0) || secrets() != 2 {
		t.Errorf("after a rotation with no grace: %d secrets", secrets())
	}
	if got := s.Snapshot().Keys["agent"].Secrets; len(got) != 2 || !slices.ContainsFunc(got, func(v SecretView) bool { return v.ValidUntil == testNow+5000 }) {
		t.Errorf("secrets view %+v", got)
	}

	commit(t, s, Change{Summary: "keys revoke agent", Kind: KindKey, Name: "agent", Mutate: func(st *State) (map[string]string, error) {
		st.Revoke("agent", testNow)
		return nil, nil
	}})
	if ok(fourth, 5000) || secrets() != 0 || s.Snapshot().Keys["agent"] != nil {
		t.Error("a revoked key still authenticates or keeps a secret")
	}
	if _, ok := s.Snapshot().Authenticate(first, at(0)); ok {
		t.Error("a revoked key's token is accepted")
	}
}

// TestAllows: a listed alias, every alias with ["*"], nothing with [].
func TestAllows(t *testing.T) {
	for _, tc := range []struct {
		models []string
		alias  string
		want   bool
	}{
		{[]string{"fast"}, "fast", true},
		{[]string{"fast"}, "deep", false},
		{[]string{"*"}, "deep", true},
		{[]string{}, "fast", false},
		{nil, "fast", false},
		{[]string{"fast", "*"}, "deep", false}, // "*" counts only alone, as Validate requires
	} {
		k := &Key{Spec: domain.Key{Models: tc.models}}
		if got := k.Allows(tc.alias); got != tc.want {
			t.Errorf("%q allows %s: %v", tc.models, tc.alias, got)
		}
	}
}

// TestRotationAudit: the audit row of a rotation holds public ids and
// valid_until only, never a hash or a token.
func TestRotationAudit(t *testing.T) {
	s, st := newService(t)
	var token string
	commit(t, s, Change{Summary: "seed", Kind: "configuration", Mutate: func(st *State) (map[string]string, error) {
		seed(st)
		return nil, nil
	}})
	res := commit(t, s, Change{Summary: "keys rotate k", Kind: KindKey, Name: "k", Mutate: func(st *State) (map[string]string, error) {
		var err error
		token, err = st.Rotate("k", testNow, testNow+3_600_000)
		return map[string]string{"k": token}, err
	}})
	if len(res.Diff) != 1 || res.Diff[0].Action != ActionRotated || res.Secrets["k"] != token {
		t.Fatalf("result %+v", res)
	}
	audit := rows(t, st, "SELECT previous_value, new_value FROM audit WHERE action = 'keys rotate k'")
	id, _ := auth.Parse(token)
	want := `{"public_id":"aaaaaaaaaaaa"}`
	if len(audit) != 1 || !strings.HasPrefix(audit[0], "["+want+"] ") || !strings.Contains(audit[0], `"public_id":"`+id+`"`) ||
		!strings.Contains(audit[0], `"valid_until":1790003600000`) || strings.Contains(everything(t, st), token[26:]) {
		t.Errorf("audit %q", audit)
	}
}
