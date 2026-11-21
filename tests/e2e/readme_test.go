package e2e

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode"
)

// shBlocks returns the sh blocks under the "## <section>" heading of
// README.md, up to the next "## " heading, each as its lines.
func shBlocks(t *testing.T, section string) [][]string {
	t.Helper()
	var blocks [][]string
	in, block := false, -1
	for _, line := range strings.Split(sectionText(t, section), "\n") {
		switch {
		case !in && line == "```sh":
			in, block = true, len(blocks)
			blocks = append(blocks, nil)
		case in && line == "```":
			in = false
		case in:
			blocks[block] = append(blocks[block], line)
		}
	}
	return blocks
}

// sectionText returns README.md from the "## <section>" heading to the next
// "## " heading.
func sectionText(t *testing.T, section string) string {
	t.Helper()
	readme := readFile(t, "../../README.md")
	_, text, ok := strings.Cut(readme, "\n## "+section+"\n")
	if !ok {
		t.Fatalf("README.md has no ## %s", section)
	}
	text, _, _ = strings.Cut(text, "\n## ")
	return text
}

func readFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// slug is the anchor GitHub gives a heading: lowercase, every rune that is
// not a letter, a digit, a space, - or _ dropped, spaces to -.
func slug(heading string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(heading) {
		switch {
		case r == ' ':
			b.WriteRune('-')
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// TestReadmeInstall pins the install blocks of README.md (F1): the root block
// first, with no sudo, then the sudo block, then the clone of this module,
// each after its lead line, and no line holding #, which stock zsh passes on
// as arguments.
func TestReadmeInstall(t *testing.T) {
	b := shBlocks(t, "Install")
	if len(b) < 3 {
		t.Fatalf("## Install has %d sh blocks, want at least 3", len(b))
	}
	module := ""
	for _, line := range strings.Split(readFile(t, "../../go.mod"), "\n") {
		if m, ok := strings.CutPrefix(line, "module "); ok {
			module = strings.TrimSpace(m)
		}
	}
	want := [][]string{
		{"apt-get update && apt-get install -y ca-certificates curl git", "curl -fsSL https://get.docker.com -o get-docker.sh && sh get-docker.sh"},
		{"sudo apt-get update && sudo apt-get install -y ca-certificates curl git", "curl -fsSL https://get.docker.com -o get-docker.sh && sudo sh get-docker.sh", `sudo usermod -aG docker "$USER"`},
		{"git clone https://" + module + ".git", "cd stargaite", "./setup.sh"},
	}
	for i, w := range want {
		if !slices.Equal(b[i], w) {
			t.Errorf("install block %d:\n%q\nwant\n%q", i+1, b[i], w)
		}
		for _, line := range b[i] {
			if strings.Contains(line, "#") {
				t.Errorf("install block %d has a line with #, which zsh passes on as arguments: %q", i+1, line)
			}
		}
	}
	text, last := sectionText(t, "Install"), -1
	for _, s := range []string{"**As root**", b[0][0], "**As a user with `sudo`**", b[1][0], "Then, as root or as that user:", b[2][0]} {
		i := strings.Index(text, s)
		if i <= last {
			t.Fatalf("## Install: %q is missing or out of order", s)
		}
		last = i
	}
}

// TestReadmeSubcommands checks that the subcommand table of README.md names
// exactly the subcommands of setup.sh's usage, help aside.
func TestReadmeSubcommands(t *testing.T) {
	usage := readFile(t, "../../setup.sh")
	_, usage, ok := strings.Cut(usage, "\nsubcommands:\n")
	usage, _, _ = strings.Cut(usage, "\n\n")
	if !ok {
		t.Fatal("setup.sh's usage has no subcommands:")
	}
	row := regexp.MustCompile(`^  ([a-z]+(, [a-z]+)*)( |$)`)
	var got, want []string
	for _, line := range strings.Split(usage, "\n") {
		if m := row.FindStringSubmatch(line); m != nil {
			for _, name := range strings.Split(m[1], ", ") {
				if name != "help" {
					want = append(want, name)
				}
			}
		}
	}
	name := regexp.MustCompile("`([a-z]+)`")
	table := false
	for _, line := range strings.Split(sectionText(t, "Scripts and agents"), "\n") {
		if !strings.HasPrefix(line, "|") {
			if table {
				break // the first table only
			}
			continue
		}
		table = true
		cell := strings.Split(line, "|")[1]
		for _, m := range name.FindAllStringSubmatch(cell, -1) {
			got = append(got, m[1])
		}
	}
	slices.Sort(got)
	slices.Sort(want)
	if len(want) != 16 || !slices.Equal(got, want) {
		t.Errorf("README's subcommands %q, setup.sh's %q (16 expected)", got, want)
	}
}

// TestDocLinks checks every relative link of README.md and docs/*.md outside
// fenced blocks: its file exists and its anchor is a heading of that file;
// then that every docs/console/*.svg is an image README.md shows.
func TestDocLinks(t *testing.T) {
	docs, err := filepath.Glob("../../docs/*.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range append([]string{"../../README.md"}, docs...) {
		_, links := mdScan(t, f)
		for _, u := range links {
			if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "mailto:") {
				continue
			}
			p, a, _ := strings.Cut(u, "#")
			if p == "" {
				p = f
			} else {
				p = filepath.Join(filepath.Dir(f), p)
			}
			if _, err := os.Stat(p); err != nil {
				t.Errorf("%s: a link to %s, which does not exist", f, u)
			} else if a != "" && strings.HasSuffix(p, ".md") {
				if anchors, _ := mdScan(t, p); !anchors[a] {
					t.Errorf("%s: a link to %s, whose anchor is no heading there", f, u)
				}
			}
		}
	}
	svgs, err := filepath.Glob("../../docs/console/*.svg")
	if err != nil || len(svgs) == 0 {
		t.Fatalf("no docs/console/*.svg: %v", err)
	}
	readme := readFile(t, "../../README.md")
	for _, s := range svgs {
		image := regexp.MustCompile(`!\[[^\]]+\]\(` + regexp.QuoteMeta(strings.TrimPrefix(s, "../../")) + `\)`)
		if !image.MatchString(readme) {
			t.Errorf("%s is shown by no image of README.md", s)
		}
	}
}

var (
	mdHeading = regexp.MustCompile(`^#{1,6} (.+)$`)
	mdLink    = regexp.MustCompile(`\]\(([^)\s]+)\)`)
)

// mdScan returns the anchors of a Markdown file's headings and the targets
// of its links, both outside fenced blocks.
func mdScan(t *testing.T, name string) (anchors map[string]bool, links []string) {
	t.Helper()
	anchors, fenced := map[string]bool{}, false
	for _, line := range strings.Split(readFile(t, name), "\n") {
		if strings.HasPrefix(line, "```") {
			fenced = !fenced
		}
		if fenced || strings.HasPrefix(line, "```") {
			continue
		}
		if m := mdHeading.FindStringSubmatch(line); m != nil {
			anchors[slug(m[1])] = true
		}
		for _, m := range mdLink.FindAllStringSubmatch(line, -1) {
			links = append(links, m[1])
		}
	}
	return anchors, links
}
