package auth

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"regexp"
	"strings"
	"testing"
)

// shape is the token of SEC-1 and design section 7: the prefix, 12 and 52
// characters of the lowercase base32 alphabet, 78 characters in all.
var shape = regexp.MustCompile(`^sk-stargaite-([a-z2-7]{12})-[a-z2-7]{52}$`)

func TestIssue(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		token, id, hash, err := Issue(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		m := shape.FindStringSubmatch(token)
		if m == nil || len(token) != 78 || m[1] != id {
			t.Fatalf("token %q (%d characters), public id %q", token, len(token), id)
		}
		if hash != sha256.Sum256([]byte(token)) || !Equal(token, hash) {
			t.Fatalf("hash of %q", token)
		}
		if seen[id] {
			t.Fatalf("public id %q issued twice", id)
		}
		seen[id] = true
	}
}

// TestAlphabet feeds every byte value through both parts: only a-z and 2-7
// come out, whatever the random bytes are.
func TestAlphabet(t *testing.T) {
	var all []byte
	for b := range 256 {
		all = append(all, byte(b))
	}
	used := map[rune]bool{}
	for i := 0; i+40 <= len(all); i++ {
		token, _, _, err := Issue(bytes.NewReader(all[i : i+40]))
		if err != nil {
			t.Fatal(err)
		}
		if !shape.MatchString(token) {
			t.Fatalf("token %q", token)
		}
		for _, r := range strings.TrimPrefix(token, Prefix) {
			used[r] = true
		}
	}
	if len(used) != 33 { // 32 letters and digits, and the "-" between the parts
		t.Errorf("%d distinct characters, want 33", len(used))
	}
}

func TestShortRandom(t *testing.T) {
	if _, _, _, err := Issue(bytes.NewReader(make([]byte, 39))); err == nil {
		t.Error("Issue with 39 random bytes: no error")
	}
	if _, err := Secret(bytes.NewReader(make([]byte, 31))); err == nil {
		t.Error("Secret with 31 random bytes: no error")
	}
}

func TestEqual(t *testing.T) {
	token, _, hash, err := Issue(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, other := range []string{"", token[:len(token)-1], token + "a", strings.ToUpper(token)} {
		if Equal(other, hash) {
			t.Errorf("Equal(%q) is true", other)
		}
	}
}
