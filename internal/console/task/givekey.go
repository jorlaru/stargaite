package task

import (
	"context"
	"strconv"
	"strings"

	"github.com/jorlaru/stargaite/internal/console/host"
	"github.com/jorlaru/stargaite/internal/console/proxy"
	"github.com/jorlaru/stargaite/internal/console/ui"
)

// giveSteps is Give an app or agent its key's step table.
func giveSteps() []ui.Step {
	return []ui.Step{{ID: "aliases", Title: "aliases"}, {ID: "name", Title: "name"}, {ID: "limits", Title: "limits"},
		{ID: "key", Title: "key"}, {ID: "try", Title: "try"}}
}

// GiveKey makes a client key for one or more aliases, shows its secret
// once, then tries it with one announced paid call and says how a client
// calls it.
func GiveKey(ctx context.Context, u *ui.UI, px *proxy.Proxy, h *host.Host) (string, error) {
	r := &run{u: u, px: px, h: h}
	res, err := r.giveKey(ctx)
	return res, r.kept(ctx, err)
}

func (r *run) giveKey(ctx context.Context) (string, error) {
	if err := ready(ctx, r.u, r.h, "aliases"); err != nil {
		return "", err
	}
	if err := r.listModels(ctx); err != nil {
		return "", err
	}
	q := ui.Question{ID: "alias", Flag: "alias", Step: "aliases", Prompt: "Which aliases may it call?"}
	if len(r.models) == 0 && r.u.Asks() && !r.u.Given(q) {
		return "No alias yet: Make an alias makes one.", nil
	}
	aliases, err := r.pickAliases(ctx, q)
	if err != nil {
		return "", err
	}
	if err := r.listKeys(ctx); err != nil {
		return "", err
	}
	name, ended, err := r.keyName(ctx)
	if err != nil || ended != "" {
		return ended, err
	}
	rpm, err := r.u.Text(ctx, ui.Question{ID: "rpm-limit", Flag: "rpm-limit", Step: "limits", Prompt: "the requests a minute the key may send",
		Default: "60", HasDefault: true, Check: positive})
	if err != nil {
		return "", err
	}
	n, _ := strconv.Atoi(rpm)
	toward := strings.Join(aliases, ", ")
	say(ctx, r.u, "creating key "+name+", which may call "+toward)
	var flags []string
	for _, a := range aliases {
		flags = append(flags, "--model", a)
	}
	secret, err := r.newSecret(ctx, "keys create", append(flags, "--rpm-limit", strconv.Itoa(n)), name, "made") // the CLI refuses "010"
	if err != nil {
		return "", err
	}
	tried, err := r.handOver(ctx, name, "made", secret, aliases)
	switch {
	case err != nil:
		return "", err
	case !tried:
		return "Done: key " + name + ", which may call " + toward + "; not tried.", nil
	}
	return "Done: key " + name + ", which may call " + toward + ".", nil
}

// keyName asks the new key's name. A name that is listed ends the run with
// no key made (ended): at once from the flags or without a terminal; at a
// terminal after asking for another name, three times at most.
func (r *run) keyName(ctx context.Context) (name, ended string, err error) {
	q := ui.Question{ID: "key-name", Flag: "key-name", Step: "name", Prompt: "a name for the key", Default: "my-app", HasDefault: true, Check: nonEmpty}
	again := ui.Question{ID: "key-name-again", Step: "name", Prompt: "another name, or Enter to make no key", HasDefault: true}
	const noKey = "No key made: Change a key rotates a key that exists."
	name, err = r.u.Text(ctx, q)
	for asked := 0; err == nil; asked++ {
		_, listed := r.key(name)
		switch {
		case !listed:
			return name, "", nil
		case r.u.Given(q) || !r.u.Asks():
			return "", "key " + name + " exists, and its secret cannot be shown again: no key made and no test call; Change a key rotates it", nil
		}
		say(ctx, r.u, "key "+name+" exists, and its secret cannot be shown again")
		if asked == 3 {
			return "", noKey, nil
		}
		if name, err = r.u.Text(ctx, again); err == nil && name == "" {
			return "", noKey, nil
		}
	}
	return "", "", err
}
