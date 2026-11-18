# Phase 7 design: the guided console

Status: draft for the owner, revision 3, after two review rounds (each: four Fable reviewers and four Fable verifiers; 63 then 61 findings kept, one refuted) and the owner's answers to eleven questions (2026-11-18). It builds on the code at `b781172` and on `docs/design/phase-6.md` (cited as "phase-6 §M"). Its inputs are the owner's live check of phase 6 on a clean Debian 13 machine (2026-11-18), run as an external reader following the README on GitHub ("the live check"), and the conversation that followed, in which the owner chose the console's shape from mockups. Code is cited as `path:line` of `b781172`. Nothing here is built or measured yet; every claim about a library comes from a probe build (§12), not from use.

## 1. Why

The live check reached a first answer with `./setup.sh`, then judged it, in the owner's words: "esto no es usable. Así de claro". The assistant works; using it does not. A line-by-line question script cannot be made pleasant, so the findings are resolved by a different tool, not patched one by one. The owner asked for a console as usable as interactive terminal tools like Claude Code's, designed from the reader's experience.

| Finding | What happened | Root cause | Resolved by |
|---|---|---|---|
| F1 | As root on a minimal Debian, the README's install block was pasted whole: `sudo: command not found`, then `curl: command not found` | The root variant is a sentence below the block (`README.md:106`) | README: one block per case, root first (C65) |
| F2 | Adding a provider warned `OPENAI_API_KEY does not resolve` before asking for that key | Every finding of every CLI answer printed (`setup.sh:312`) | Warnings held while a later step of the task resolves them (§3) |
| F3 | 98 model ids, unsorted, speech, image, embedding and moderation mixed with chat | The list shown as it comes (`setup.sh:571-585`) | Model pick by vendor and family, chat only unless asked (§3) |
| F4 | Two warnings in rule wording, three times in one install, then on every later run | Findings of the whole configuration after each command | As F2; the rest in "Check everything" |
| F5 | The printed example `curl` has no `max_tokens` | `setup.sh:1071` | The key screens print base URL, alias and an example with `max_tokens` (§4.3) |
| F6 | `key` asked `a name for the alias your clients call [main]`; the reader typed a key name three times; `main` did not exist | One free-text question both creates and picks an alias (`setup.sh:636`) | The key task picks aliases from a list; naming the key is its own step |
| F7 | After Ctrl-C at a question, the next paste reached bash as `[200~…` | Unknown | Terminal modes and termios asserted after Ctrl-C, SIGTERM, SIGHUP and a panic (§10, layer 4) |
| F8 | No fallback model for an alias | One `--target` (`setup.sh:652`); the proxy serves an ordered list (`README.md:380`) | Fallbacks from any provider in Make and Change an alias |
| F9 | A key name that exists answered `another name, or Enter to skip the first answer` | No list of keys, no rotate (`setup.sh:669`) | Change a key: rotate, aliases, limits, budget, revoke |
| F10 | `provider` ended at Compose's `Started` line | `setup.sh:537-538` | Every task ends with its result |
| F11 | Five providers took five runs, five confirmations, five recreates | One provider per run | Add providers: one multi-pick, one recreate |
| F12 | A credential written without being tried | No call before writing | Free check of the key before writing (§3) |
| F13 | Providers already added on a trailing line, still offered as new | `setup.sh:494` | Added providers marked in the pick |
| F14 | Nothing shows what exists | Cut 1 of phase-6 §6 | Home summary and "See what exists" |

## 2. Goals, non-goals, anti-goals

**The rule this phase adds:**

> **The console always guides.** A person never needs to know a command, a name or a file. They open it, see what exists and what is missing, pick what to do from a list, and are led through short steps, each choice picked from a list whenever the answer exists or a provider lists it. Free text is typed only for a new name or a value. Every task ends with its result and returns to the same question.

Goals, at the end of phase 7:

- A first run goes from a clean machine to a first answer in one guided flow with a step bar (§4.1), including optional fallback models.
- Every later change is a guided task from one home menu (§4.2, §4.3), which also suggests what is missing.
- A reader can give an alias models from different providers, in order, as fallbacks; cap spending on everything, on an alias, or on one app or agent (its key); add several providers in one go, each key tried for free before it is written.
- Models are listed sorted and grouped by vendor and family, chat models only unless asked.
- Phase 6's subcommands and the new tasks that touch the host also run from flags, with or without a terminal (§4.4, OPS-2, OPS-11).
- **The code is easy to change: a task is one plain Go function that reads as its steps, top to bottom (§5). The owner stated this as vital, on par with the screen.**
- A cancelled paid check stops at the server, not only in the console (§3, owner question 9).
- The README shows the console with images rendered from its own tests.

Non-goals: managing a remote StargAIte from another machine; running on macOS or Windows (the console compiles there, as `make ci` builds every package for `darwin`, `Makefile:9`, but runs on Linux, as phase 6); a web UI; new proxy capabilities beyond the one CLI flag of §3 (Paid cancel); translating the console (English, PUB-8); flags for tasks the `stargaite` CLI already does without questions (§4.4); stepping back one question inside a task (§3 Esc).

Anti-goals (a failure even if it works):

- A console that holds rules the proxy already holds (validation, prices, budget reach, defaults): two places to keep in step.
- A new door into the proxy: no admin port published, no Docker socket in a container.
- New dependencies in the proxy binary or its image.
- A provider or telemetry credential on screen, in argv, in a log, in the scrollback, or in any file but `.env`. The client key is the one secret shown, once, by design (§3).
- A paid call without the reader's explicit order (an Enter on a screen that states its count, or naming the task in flags mode, as phase 6), any retry of one, or one that a cancel leaves running at the server.
- Code a newcomer cannot follow: logic split across layers, flows hidden in callbacks.

## 3. Decisions

| Decision | Choice | Reason | Discarded |
|---|---|---|---|
| Shape | Always guided: home menu, then a task with a step bar, back to the menu with the result (owner's choice, from mockups) | The owner: guidance closes every configuration possibility without the reader knowing what to do | A `/` command prompt as the main view; a full-screen panel with sections |
| Where it runs | On the host, as its own static binary, `stargaite-console` | Host tasks (`.env`, Compose, TLS files, upgrade) cannot run in the container without the Docker socket (phase-6 §2) | A command inside the proxy binary |
| A separate binary | `cmd/stargaite-console`, same repository and module; the proxy binary does not import it, checked by `make check` against a committed module list (§5) | The proxy and its image keep their dependencies, size and start-up (owner: minimise attack surface and maintenance) | Inside `cmd/stargaite` |
| TUI library | Bubble Tea v2 and Lip Gloss, pinned. The pick and the single-line input are written here if C59's probe shows the input stays under about 80 lines with a wide-character test and dropping Bubbles removes `atotto/clipboard`; otherwise Bubbles' text input is kept and §12 says why | Resize, Unicode width, terminal kinds and keys are solved there; fewer modules where our own piece is small | Hand-written terminal I/O throughout |
| Screen mode | Inline: one long-lived program for the session; each change prints one permanent transcript line before it is made; the live view holds only the current step | Copying works over ssh; the transcript records what was done | Alternate screen throughout |
| The client key | Shown on the alternate screen, that step only, until "I have stored it", then the console returns to the inline view, so the key never reaches the scrollback (owner, question 10). Every exit path (SIGTERM, SIGHUP, the recover) leaves the alternate screen first; Ctrl-C on that screen asks first. In flags mode the key is printed once, alone, on standard output, as phase 6 (`setup.sh:677-681`). If Bubble Tea v2 cannot switch screens for one step, C59 reports it to the owner before anything else is built | A secret shown once must not stay in the terminal's history, yet must not be lost by accident | Inline with a cleared view (cannot guarantee the scrollback) |
| How it reaches the proxy | `docker compose exec -T stargaite /stargaite <words> --json <flags> -- <positionals>`, built and run by one function, `px.Call(ctx, &reply, words, flags, positionals...)`, whose `Runner` is `host.Exec` in production and `cli.Main` in tests. Under TLS it adds the two `-e` values of phase-6 §2, read from the override's `# setup:` lines | No new port, API or credential; the CLI validates every change and names what to fix (OPS-5) | Publishing the admin listener on loopback |
| Decoding | Each reply struct lists its required keys; one generic decode fails when one is absent, and that failure is the only version refusal, naming "Rebuild" (below) | Decoding is the real contract between the two binaries | Version equality |
| Bootstrap | `./setup.sh`: bash version → root rule → Docker reachable → export the console when `bin/stargaite-console` is missing or `--version` differs from `git describe --tags --always --dirty` (or either is `dev` or ends in `-dirty`) → `exec bin/stargaite-console "$@"`. The export needs only Docker and comes from the console's own Dockerfile stage (`FROM scratch`, one file) placed before the proxy stage, which stays last; how it is exported (`docker build --output`, or `docker create` plus `docker cp`) is settled in C59 on Compose 2.24.4. Everything else (the full machine report, the `.env` port line and port pick, the first image build with Compose's progress) is the console's Machine step | `help`, a usage error and `status` must not need a build; the port can be picked where it is asked | Keeping the build in bash |
| The console's own guards | At start, `host/` resolves the clone as the parent of the executable's directory, requires `compose.yaml` there, changes to it, and applies the root rule (uid 0 refused on another user's clone); again before Upgrade's fetch | The binary can be run directly, past the bootstrap | Guards in bash only |
| Version | The home line names both versions; on a mismatch it suggests "Rebuild to this clone's version" | Development and ordinary states must not be blocked | Refusing every change on a mismatch |
| Rebuild | The Upgrade task entered after its fetch-and-merge steps, sharing every later step (previous image, backup, start, check, the way back) | One path, one set of guarantees | A second upgrade path |
| Paid consent | At a terminal, an Enter on the paid step, accepted only after the step has been drawn and a fixed 400 ms have passed, with paste ignored on paid, confirm and pick pieces and pending input flushed at start. In flags mode, naming the task is the consent, as in phase 6; the count is printed first (owner, question 2) | A buffered Enter or a pasted newline cannot pay; the README, CI and scripts keep working | A new required flag |
| Paid calls, one door | A leaf package `paid`: `Open(calls int) (*Budget, error)` refuses a count above the step's ceiling and a session total above **12 paid calls per console run**; `(*Budget).Take(n)` debits. `proxy.Verify(ctx, b, target)` and `host.TryCompletion(ctx, b, …)` begin with `b.Take` and fail on a nil or spent budget; more calls in a verify answer than taken is a loud error. `ui.Paid` is the only non-test caller of `paid.Open`, checked by the imports test. The test completion has a **30 s** timeout and a fresh HTTP client with keep-alives off. No retry path exists; pressing the step again is a new, re-announced paid step | "Counted, capped, never retried" true by construction | Convention in each task |
| Paid count | A check: the sum over the unverified targets it covers of one call per declared level, plus one at `none` when the target can disable reasoning, or one call when it declares neither (`README.md:838-840`, as `setup.sh:619` counts); a verified target is skipped unless the reader asks. A try through an alias with N targets: "up to N calls, one per target if the earlier fail" | The stated count is the maximum billed | One per step |
| Paid cancel | Esc or Ctrl-C during a check cancels it **at the server**: `targets verify` gains one flag, `--until-stdin-closes`, with which the CLI ends its admin request when its standard input closes; the console runs that exec with a pipe on standard input and closes it on cancel, and the server, which already stops a verify run when its request's context ends (`internal/api/verify.go:113-116`, `internal/admin/verify.go:55`), makes no further call. No further target's check starts. The screen says what may still be billed (the call in flight) (owner, question 9) | The owner's rule: cancelling the client must stop the work; `docker compose exec` does not forward a signal to the process it started | Bounded but not cancelled, as phase 6 |
| Step order around a check | Every step that edits a target or its provider (prices, a timeout, a plan) comes before that task's check. At a terminal, a task that edits a verified target ends with "N targets must be checked again: M paid calls", "Check it" or "Not now". In flags mode the re-check runs only with `--reverify` (`setup.sh:1114`); without it the task prints the line and exits 0 | Any target or provider edit clears `verified_at` (`internal/config/storage.go:273-288`); flags mode never pays for what phase 6 did not | Checking first |
| Try it | Only where a fresh client key is in hand: the first run, "Give an app or agent its key" and its rotate (owner, question 1) | The console holds no client key; the data plane needs one (`internal/api/api.go:109-130`) | A temporary key per try; a new admin route |
| Esc and Ctrl-C | On a question: Esc leaves the task for the home menu, with a line naming what was already changed and kept; reopening a task shows what exists, read from the proxy. There is no step back. A paid step offers "Check it" or "Not now". During a running step: Esc cancels it (a paid check at the server, as above; external commands by SIGINT to their process group, killed after 10 s) and leaves the task. Ctrl-C at home quits; inside a task it does what Esc does, then quits; exit codes 130 and 143 as phase 6 (owner, question 4) | A straight-line task cannot go back without history; resumable as phase 6 | A replaying runner |
| Models | Grouped by vendor and family from one table of patterns; unknown ids go to `Other`; non-chat ids hidden unless the reader asks for all; OpenRouter grouped by vendor first (its ids carry it) | A provider's list gives ids only; the table degrades to `Other`, never to an error | A model catalog in the repository |
| Prices | Never read from a provider's list (ADR-012, owner question 6). The console asks the proxy which prices a change needs: `apply --dry-run -f -` with the intended alias or budget (`internal/cli/cli.go:308-324`), or the budget's refusal, as phase 6 does (`setup.sh:740-780`); it then asks exactly the prices the answer names, before any check, in the first run too | The proxy owns the budget-reach rule; the console asks only what it must | Computing the rule in the console |
| Reasoning | No question. After the alias exists, setting an effort shows what will be sent for it (`models explain <alias> --effort <level>`, `internal/cli/cli.go:254-259`); `reasoning_undeclared` stays in "Check everything", in plain words, with its CLI command | Nothing new to map; the reader sees the real effect | Asking "does this model think?" |
| Home data | Before the menu: one bounded, concurrent fetch of `export`, `validate`, `targets list`, `providers list` and `version`, behind a spinner; if the bound expires, the menu without Suggested and a line saying the proxy was not reached. A stopped or unready container shows "stopped" and offers Start | A blocking pick cannot change once shown | Filling the summary in later |
| Suggestions | A table of rules over that data and local files: no provider, no alias, no key (each reopens the first run at that step); a provider's key missing (`credential_unresolved`); an alias without a fallback; no spending cap; the warnings `unpriced` and `no_fallback_prices` (`internal/config/validate.go:450-456`); a provider none of whose targets is verified; a version mismatch; an upgrade available only after the reader ran Upgrade's check in this session. The home makes no network call beyond the proxy | One place to add a suggestion; no state store | Persisting "not checked"; fetching on every open |
| Warnings | Shown in the task that touched the resource, in plain words with the task that fixes it. A finding that a later step of the running task resolves (`credential_unresolved` before the `.env` write and recreate, `target_unverified` before the check) is held, from one table of (code, resolving step) in `rules/`, and shown only if still present when the task ends. A code without a row shows the CLI's own message and command; a test parses `internal/config` for `r.warn(` codes and fails on one without a row | F2, F4 | Printing every finding |
| Credential check | Phase 6's order: `providers add` (the CLI validates the URL), then the key, typed hidden, then one free model-list request from `host/http.go` (§5), to the provider's base-URL host or its host-side form for `host.docker.internal`, private only when the provider's stored `allow_private_network` says so. 401 or 403: asked again, at most 3 times (in flags mode, exit 1 before `.env` is written). Other answers: written, said "not checked" for this session; reopening the provider checks again. OpenRouter's list is public: its key is "not checked" unless C59 finds an endpoint in OpenRouter's current documentation that needs the key, recorded in one console table with its test | F12, without a paid call | Writing first |
| Untrusted text | Two functions in the Bubble Tea-free `ui` core: one-line (newline and tab become a space; ids, names, messages) and multi-line (logs). Both strip C0 and C1 controls, ESC, DEL, invalid bytes, and the bidi and zero-width ranges, and cap length. Applied in `UI.Say` and `UI.Show` for every front, and where `proxy/` and `host/http.go` hand strings back | The screen carries paid confirmations and the client key | Rendering as given |
| Libraries' ambient behaviour | `main` picks guided only when standard input and output are both terminals, before any program exists; the program runs with an allow-listed environment (`TERM`, `COLORTERM`, `NO_COLOR`, `LANG`, `LC_*`) and a fixed colour profile, so nothing is detected; no paste or clipboard binding anywhere; the console never reads or writes the clipboard | No trace file, no clipboard tool, no surprise prompt | Defaults |
| Terminals | Works at 80×24 (lists window to the height), over ssh, inside tmux, with `NO_COLOR`; state marked with ✓ ✗ and words, never colour alone | Common real settings | — |
| Uninstall | `docker compose down` with neither `-v` nor `--rmi`, then `docker rmi` of the two tags without `-f` (a tag in use is refused and reported), then `bin/stargaite-console`; prints the commands that remove the volume, `.env`, `backups/` and `tls/` (owner, question 5; OPS-11 forbids removing a volume) | OPS-11 unchanged | A data-removing option |
| Upgrade | Phase 6's order verbatim: check and refusals, what changes, confirm, keep the previous image, fast-forward merge, build, backup, start, check, the way back. Only after the post-upgrade check passes, and only in guided mode, the console exports the new console over `bin/` and re-execs it once with no arguments, after its program has quit, with a marker that refuses a second re-exec in the session; in flags mode it exits with the task's status. `--legacy-backup` is kept | OPS-7 and OPS-11's guarantees (phase-6 §2) | A new order |
| Size | The console has its own ceiling, outside the kick-off's 20,000 lines of production code (`kick-off.md:53`, `:902`), which protect the proxy's core; production Go is 15,963 lines today. Estimate 5,000 to 6,000 lines. The ceiling is set after C59a's skeleton is measured, with a cut order: See logs, Stop or start, Uninstall, then See usage and cost (owner, question 11) | The console is a host tool, not the core | Counting it inside 20,000 |
| Language | English on screen (PUB-8) | — | — |

## 4. What the reader sees

The owner chose these screens from mockups kept with the design's scratch files (`consola-siempre-c.png`, `consola-menu.png`); model names in them are placeholders, and their footers' "esc back" reads "esc menu" (§3). Three tasks were added after that choice, by the review: See logs, Stop or start, Uninstall.

### 4.1 First run

`./setup.sh` when the proxy has no provider, no alias or no key. The step bar is **machine → provider → model → fallbacks → check → alias → key → first answer**, and it starts at the first missing step, earlier ones shown done, so an interrupted first run resumes; the provider step is done only when its credential resolves. Machine reports everything missing at once, offers another port when 4000 is taken, and builds and starts the container. Provider, model and fallbacks (from any provider, at most two, adding a provider inline if needed) are picked; prices are asked only if the proxy says a budget needs them; the check states its paid calls and waits for Enter; the alias defaults to `main`; the key screen shows the secret until "I have stored it"; the first answer is one paid try, announced, with Enter, then base URL, alias and an example call with `max_tokens`. It ends on the home menu. A key not confirmed as stored is recovered by rotate, and the home line says so after such an exit.

### 4.2 Home

A summary line (address, readiness, both versions; counts of providers, aliases, keys; "spend not capped" when true), the result of the last task, then **What do you want to do?** with **Suggested** first (§3), then the tasks in groups. Typing filters; the list windows to the terminal's height. Ctrl-C quits. Until C64, the menu lists only the tasks already ported (§11).

| Group | Tasks |
|---|---|
| Providers | Add providers · Change a provider |
| Models your apps call | Make an alias · Change an alias |
| Apps and agents | Give an app or agent its key · Change a key |
| Spend and traces | Cap spending · See usage and cost · Send traces |
| This machine | See what exists · Open to the network · Upgrade · Back up or restore · See logs · Stop or start · Check everything · Uninstall |

### 4.3 Tasks and their steps

**[apply]** marks the steps that change something; **[paid]** the steps that call a provider for money, each stating its count, "at most 256 output tokens, never retried", with "Check it" (or "Try it") and "Not now".

| Task | Steps |
|---|---|
| Add providers | which (multi-pick; presets, added ones marked, or another compatible service) → for each: its URL, protocol and credential variable when the preset lacks them → `providers add` **[apply]** → for a local service, the `host.docker.internal` notice, the override line and the private-network confirmation, as phase-6 §2 → key, hidden (skipped when the provider has none) → free check → `.env` line **[apply]** → after all: recreate once, confirmed **[apply]** → done |
| Change a provider | which (re-checks its key for free) → replace key **[apply]**, then recreate, confirmed **[apply]** · own timeout **[apply]** · subscription plan **[apply]** · remove: names the aliases using its targets, refuses (naming them) if one would be left with none, otherwise confirms and takes the targets out of those aliases, removes the targets and the provider, removes its variable from `.env` when nothing else references it, recreates once **[apply]**. Edits end with the re-check step of §3 |
| Make an alias | name → model (by family) → fallbacks, from any provider, in order, at most two → prices the proxy says a budget needs **[apply]** → add targets **[apply]** → check **[paid]** → output cap when a target needs one → make the alias **[apply]** → effort, with what will be sent for it **[apply]** → done |
| Change an alias | which → add a fallback (→ check **[paid]**) · remove a fallback · reorder · effort, with what will be sent · output cap · see what it sends · remove the alias; each change **[apply]** |
| Give an app or agent its key | the aliases it may call (multi-pick) → name → requests a minute → create **[apply]** → shown until "I have stored it" → try it **[paid]** → base URL, alias and an example call with `max_tokens` → done |
| Change a key | which → rotate **[apply]**: old secret valid for 1 h or ended now, new one shown until stored, then try it **[paid]** · aliases it may call **[apply]** · limits (requests a minute, requests at once) **[apply]** · budget **[apply]** · revoke **[apply]**, confirmed |
| Cap spending | on what: everything · an alias · an app or agent → limit in USD → resets: daily · monthly · never → prices the proxy says it needs **[apply]** → the budget **[apply]** → re-check step of §3 · remove a cap **[apply]**, confirmed |
| See usage and cost | period → by key, alias or model → table |
| Send traces | Langfuse or OTLP → endpoint (local endpoint handled as a local provider) → "replaces telemetry toward <old>, none of its credentials kept", confirmed → keys, hidden → `.env` lines and recreate, confirmed **[apply]** → unset, then set **[apply]** → test → done · turn off **[apply]**, confirmed |
| See what exists | providers and credential state, aliases with targets in order, targets without an alias (with remove), keys, budgets, telemetry, versions (phase 6's `status`, extended) |
| Open to the network | addresses → your certificate or a self-signed one → files and override **[apply]** → recreate, confirmed **[apply]** → done; every TLS ownership, rootless and group rule of phase-6 §2 kept |
| Upgrade | as §3, ending with re-export and re-exec; "Rebuild" enters it after the merge |
| Back up or restore | back up now **[apply]** · restore one: the confirmation says in plain words that keys revoked since come back, spend since is forgotten and no audit record is written, and lists the returning keys when the copy can be read; a transcript line names the file and its time; stop, put back with the current image (`docker compose run --rm --no-deps -T`), start **[apply]** |
| See logs | the last lines, then follow until Esc, each line cleaned and printed to the scrollback |
| Stop or start | stop · start · restart, confirmed **[apply]** |
| Check everything | each finding in plain words; Enter opens the task that fixes it |
| Uninstall | what it removes and what it keeps (§3), confirmed **[apply]** |

### 4.4 Flags

The same task functions run with flags. At a terminal, flags pre-answer their questions (shown as done) and the rest is asked; the task exits with its code instead of returning to the home menu. Without a terminal, an answer with a default takes it, a missing one exits 2 naming its flag, a change that replaces something or interrupts traffic needs `--yes`, and a paid step prints its count and runs (§3).

| Subcommand | Task | Flags |
|---|---|---|
| `install` | First run | phase 6's (`setup.sh:1103-1121`), plus `--fallback <provider/model>[,…]` |
| `provider` | Add providers | phase 6's; `--provider a,b` (presets only) with `--key-env a=VAR,b=VAR` |
| `alias` | Make an alias; on an existing alias, `--fallback` appends and checks, without it the alias is kept | phase 6's, plus `--fallback` |
| `key` | Give an app or agent its key | phase 6's |
| `budget`, `telemetry`, `remote`, `upgrade` | Cap spending, Send traces, Open to the network, Upgrade | phase 6's |
| `status` | See what exists, as text | `--check` |
| `stop`, `start`, `restart`, `logs [--follow]`, `backup`, `restore --from <file>`, `uninstall` | the machine tasks | `--yes` where they interrupt or replace |
| `help` | usage, naming the `stargaite` command for the proxy-only actions without flags here (reorder an alias, limits, usage, explain) (owner, question 7) | — |

### 4.5 Phase 6 behaviours

| Behaviour | Fate |
|---|---|
| Machine check, root rule, bash 4.4 | Bash keeps the bash version, root rule and Docker check (`TestSetupRoot` stays, `TestSetupPrereqs` split); the full report moves to the Machine step; curl and jq leave the prerequisites in C64 if nothing on the host needs them, with README and `TestSetupPrereqs` updated |
| `--port` and `STARGAITE_PORT` | Kept; the Machine step offers another port when 4000 is taken |
| `.env` writer (temporary `0600` file, refused values, replace on confirmation) | Ported to `host/` with its tests |
| Override marker and regeneration from `# setup:` lines; an override without the marker never touched | Ported to `host/`; while both writers exist (C59 to C64) they share a format test |
| TLS state read from the override on every start (base URL, name to loopback, CA, admin variables on exec) | Ported to `host/`, used by `proxy/` and the test completion |
| Resume: what exists is kept, never made twice | Kept in every task |
| Bounded waits (Compose up 20 min, readiness, CLI 90 s, fetch 20 s) | Ported to `host/` and `proxy/` with the same bounds |
| Model listing, local servers at their host-side URL | Kept, now grouped (§3) |
| Budget price prompting | Kept, asked of the proxy (§3 Prices) |
| Telemetry replace-whole and its confirmation | Kept (§4.3) |
| Remote and TLS (own or self-signed, ownership, rootless refusal, renewal keeps addresses) | Kept |
| Upgrade order, previous image, backup before start, way back, `--legacy-backup`, up-to-date check against the running version | Kept |
| A paid verify left to finish within the server's bound after a cancel | Changed on purpose: cancelled at the server (§3) |
| `status` limited to versions and credentials (cut 1) | Reversed: "See what exists" |
| Printing every finding | Dropped (§3 Warnings) |

## 5. Code structure

The structure is part of the design, because changing the console must stay easy.

```
cmd/stargaite-console/        main: guards, mode, the session goroutine, exit codes, re-exec after an upgrade
internal/console/
  ui/                         the UI core (no Bubble Tea): typed questions, the answer chain, the step table,
                              the clean-text functions; the Front interface
  ui/guided/                  the Bubble Tea front: root model, bridge, step bar, alternate screen for the key
  ui/plain/                   the front without a terminal: defaults or exit 2 naming the flag (about 40 lines)
  piece/                      what the guided front draws: pick (filter, groups, multi, windowed), text, secret,
                              confirm, paid, progress, result, table, follow
  paid/                       the paid budget: Open, Take, the ceilings
  task/                       one file per task, plus shared step functions; the menu table
  rules/                      suggestions, held warnings and plain-words texts, as tables
  families/                   the model family table and the grouping function
  proxy/                      px.Call and the reply structs
  host/                       exec.go (every external command), http.go (every HTTP call with a secret),
                              .env, the override and TLS state, Compose, TLS files, git, the guards
```

**A task is a plain Go function**, `func(ctx context.Context, u *ui.UI, px *proxy.Proxy, h *host.Host) (string, error)`, read top to bottom. Shared steps are plain functions in `task/` with explicit inputs and outputs, such as `pickModel(ctx, u, px, provider) Target`, each owning its questions as `ui.Question` values; a task's step table is the concatenation of its steps' tables; a nested task runs through `u.Run(sub)`, which swaps the step bar. Loops over items are a `PickMany` followed by `for`, and per-item flags are keyed by item.

**The UI is one concrete struct**, in a package that does not import Bubble Tea. Each typed method (`Pick`, `PickMany`, `Text`, `Secret`, `Confirm`, `Paid`, `Work`, `Say`, `Show`, `Table`, `Follow`, `Reveal`) does, in order: check the question is in the task's table, take `answers[q.ID]` (from flags or a suggestion's presets), else `front.Ask(ctx, q)`, then `q.Check`. `Reveal` shows the client key: the guided front on the alternate screen until confirmed, the plain front once on standard output. Leaving a task is an error value, `ui.ErrLeave`, returned by any method when the reader presses Esc; a task returns it as is, after saying nothing more. `Front` has two methods, `Ask` and `Show`, and two implementations: `guided` and `plain`. Tests use the UI with an answers map and the plain front, which is flags mode itself.

**The session and the terminal.** `main` owns the work, not Bubble Tea: one session goroutine (`for { choice := u.Pick(menu); run(choice) }`) with a `done` channel and one `recover` (which maps a panic to exit 1 with one line, for every front); after the program's `Run` returns, `main` cancels the root context and waits for `done`, bounded at 12 s, before exiting or re-execing. One context per task derives from the root; Esc cancels only that one. One signal owner: SIGINT, SIGTERM and SIGHUP cancel the root context. A `go` statement is allowed only in one helper that recovers, checked by the imports test. The root model only renders what it is asked: header, step bar, current piece, last result, and a spinner when no piece is active.

**Processes.** `host/exec.go` runs every external command: `exec.CommandContext`, each child in its own session (no controlling terminal) with the git variables of phase-6 §2 on every git call, its process group signalled with SIGINT on cancel and killed after 10 s, phase 6's bounds, and three output modes: *display* (cleaned, with Compose's `.env: line N` errors cut after the line number), *capture* (returned to `proxy/`, never displayed or quoted in an error, even on a decode failure; only the decoded `.error.message` is shown, and for `keys create` or `rotate` a decode failure says the key exists and must be rotated), and *file* (standard output to an `O_EXCL` `0600` file under a `0700` `backups/`, or standard input from a regular file, never a terminal).

**HTTP with a secret.** `host/http.go` holds one client constructor: transport built from scratch, no proxy from the environment, no redirects followed, keep-alives off, 30 s, a size-capped read, the system roots for providers, and for the data plane a pool holding only `tls/<ca>` with the fixed dial address and server name; verification is never disabled. Three functions: `ListModels`, `TryCompletion` (behind `paid`), `Ready`.

**Secret input.** The secret piece renders nothing of the value, not even mask characters; it accepts a terminal paste as one message; it refuses what `setup.sh:139-141` refuses (empty, `'`, a newline, a trailing `\`) before returning.

Rules, each checked by a test or `make check`:

- `go list -deps` of `task/`, `proxy/`, `host/`, `paid/` and the `ui` core names no `charm.land/` path; `piece/` imports none of `task/`, `proxy/`, `host/`; only `host/` imports `os/exec` and makes HTTP calls with a secret; only `ui` calls `paid.Open`. One test reads the imports.
- The module set of `go list -deps ./cmd/stargaite` equals a committed list (today's eleven modules); any addition fails (`make check`).
- The console decides nothing the proxy decides: it decodes the CLI's JSON into its own small structs and shows the answer. Its own logic is the guidance (order of steps, suggestions, grouping) and the host layer. Tests may import the proxy's packages; the console's code does not.
- `host/` is small and is where review looks hardest: secrets, file modes, argv, ownership.
- No mutable package-level state; no `init` with effects.

## 6. Paid calls

Unchanged in kind from phase 6 §1: `targets verify` with `--max-output-tokens 256` and a test chat completion with `max_tokens: 256`, only through `paid` (§3), at most 12 per console run. Each is its own step, states its maximum count first, runs only on the reader's order, is never retried, stops the task at the first failure with the provider's reason, and is cancelled at the server by Esc or Ctrl-C. New: fallbacks add one check per new target. Layers 1 to 3 of §10 run against fakes and the mock only.

## 7. Bootstrap and delivery

As §3. During C59 to C64, `setup.sh` dispatches to the console only the subcommands already ported and keeps the bash code of the others. A bash step function and its tests stay until the last subcommand that calls it is ported: `step_provider`, `step_model` and `step_target` until C62, when `install` is ported; until then the no-subcommand entry is the bash menu and `install` is wholly bash. Each step updates the README paragraph of the subcommand it ports; C65 restructures the README and adds the images.

## 8. Live runs and their caps

The lead drives the console on the owner's test machine over ssh, from a driver script kept outside the repository:

- **Keys.** Provider keys made for this phase. Most calls go through one dedicated OpenRouter key with a **USD 1 credit limit set at OpenRouter**, restricted to cheap models: the only cap that holds if everything else fails. A cross-provider fallback is proved first with two provider entries on that key (the preset and a compatible service at the same URL); direct keys (Anthropic, Gemini, MiniMax) for at most 8 calls in all. Each key goes from the owner's secret store to the hidden prompt by `tmux load-buffer -b <name> -` from a pipe and `paste-buffer -p -d`, never in argv or on screen; after each run the pane, with its full history, is searched for every key, the patterns read from a file descriptor, before any screenshot is rendered or read. Live client keys are revoked after each run; the phase's provider keys are revoked at its end.
- **Count.** The driver keeps its counter in a file and refuses to press Enter on a paid step past the cap, with a dry-run test of that refusal: **40 paid calls for the phase**, 256 output tokens each, by step: C60 6, C61 3, C62 4, C63 3, fix rounds and re-runs 24. A re-run of a paid step beyond its step's share asks the owner first. The owner's final run is outside the 40.
- **Second layer.** A USD 1 instance budget set in that StargAIte by flags right after the container starts, with prices given for the targets used (budgets count only calls with a cost, `README.md:653`), set again after every restore or reset. Reaching USD 1 at OpenRouter or here is reported to the owner, who raises it.

The owner tests last, from a reset machine, once the live runs pass and the README describes them.

## 9. Size

Measured, not estimated, as phase 6 did (phase-6 §6): C59a's skeleton is measured and the console's ceiling set then, with its cut order (§3 Size). Each later step reports its lines against it.

## 10. Testing

1. **Pieces.** Key messages fed to each piece's `Update`, the view text asserted; no terminal. The clean-text functions with an id holding `\x1b]52;c;…\x07`, `\x1b[2J` and a bidi override; the secret piece with a pasted `$ #\` value intact, a multi-line paste refused, the value never in the view; the paid piece ignoring an Enter in the same batch as the previous answer and any paste.
2. **Tasks, in `make ci`.** Each task run by the UI with an answers map and the plain front, against the real CLI in process: `px.Call`'s runner is `cli.Main` (`internal/cli/cli.go:347`), and the admin fixture of `internal/cli/client_test.go:90` moves to a small non-test package, `internal/admintest`, imported only by tests, with verify's clients pointed at `tests/mockprovider` on loopback. `host/` runs on a temporary directory with the existing PATH shims for docker, git and openssl. Paid calls are counted at two seams, the argv of `targets verify` and a fake data plane behind `host/http.go`: Esc before Enter makes zero calls, a count above the ceiling is refused before any call, the 30 s timeout ends a try, target 2 is never requested after target 1 fails or is cancelled, a flags-mode budget with a price makes zero requests. `host/http.go` with `httptest`: a redirect is not followed, `HTTPS_PROXY` is ignored, a certificate outside `tls/` fails, the key is in no error text, one request per call. `host/exec.go`: SIGINT during a hanging `compose up` shim exits 130 within 12 s, the shim got SIGINT, no `.env.*` file remains; a shim that opens `/dev/tty` fails; a grandchild is gone after cancel; a failing docker shim whose standard output holds a secret leaks it nowhere.
3. **Guided front, in `make ci`.** The real guided front in a `tea.Program` with input disabled and output discarded at a fixed 80×24; the test sends key messages and reads frames from a test-only wrapper model. Covers the first run, a paid step (Esc: zero calls; Enter: exactly the stated count), the key screen, Esc mid-task, quit during `Work`. Golden **text** frames (ANSI stripped, covered by the PUB-2 search, fixed host name and addresses, the client key masked) are compared; the README's PNG images are rendered from them by a make target, not compared.
4. **Real terminal, in `make setup-test`.** A few runs of the whole binary in a pseudo-terminal through a terminal emulator, against the mock provider and a real container: the emulator's modes (bracketed paste, cursor, alternate screen, keyboard) and the tty's termios restored after Ctrl-C, SIGTERM, SIGHUP and a forced panic (F7); no client key in the emulator's scrollback after "I have stored it", after a resize while shown and after SIGTERM; a paste during a running step never shown; Esc during a delayed `targets verify` on the mock leaves the mock's request count at the stated count and shows the "may still be billed" line; recording shims named tmux, xclip, xsel, wl-paste and wl-copy first on PATH, inside tmux, with Ctrl-V in the text and secret pieces, are never invoked. The harness is a static test binary built in the Go image and run on the host; its modules are counted in §12; the tmux case is skipped with a printed reason when tmux is absent.

| Phase 6 test | Successor | Layer | Lands in |
|---|---|---|---|
| `TestSetupRoot` | Kept, on the bash bootstrap; the console's own guard tested in `host/` | bash, 2 | C59b |
| `TestSetupPrereqs` | Split: bash keeps Docker reachable; the Machine step's report | bash, 2 | C62 |
| `TestSetupSecrets` (argv, output and file assertions, with its leaking fixture as negative control) | `host/` writer and exec tests carrying each assertion and the negative control; layer-4 transcript and scrollback search | 2, 4 | C59b |
| `TestSetupOverride` | `host/` override tests, plus the shared-format test while bash still writes it | 2 | C59b |
| `TestSetupModels` | `families/` and Add providers task tests | 1, 2 | C59b, C60 |
| `TestSetupFlags`, `TestSetupNoTTY` | Plain-front runs of each ported task | 2 | each task's step |
| `TestSetupPrompts` | Guided-front runs of each ported task | 3 | each task's step |
| `TestSetupPaidCalls` | `paid` and paid-seam tests | 2, 3 | C59b |
| `TestSetupBounds`, `TestSetupInterrupt` | `host/exec.go` bound and cancel tests; terminal restore | 2, 4 | C59a, C59b |
| `TestSetupRemote` | Open to the network task tests | 2, 4 | C64 |
| `TestSetupUpgrade` | Upgrade task tests, failed start included; `make setup-test` upgrade and restore | 2, 4 | C64 |

An old test is deleted only in the commit where its successor is green and no bash code it covers still ships.

## 11. Ordered changes

| Id | What | Proves it | Kick-off |
|---|---|---|---|
| C59a | Walking skeleton, nothing else: `main` with the session goroutine and exit codes; the `ui` core, plain front and guided front (root model, bridge, step bar); the pieces pick, text and confirm; the alternate-screen switch for one step; one read-only task, See what exists, in both modes; the console's Dockerfile stage and its export; `setup.sh` dispatch for `status`. Probes: the export path on Compose 2.24.4, re-export over the running binary, a throwaway pick of 100 ids at 80×24 inside tmux and over ssh, the hand-written input against Bubbles' (§3). Measures its lines and sets the console's ceiling (§9) | Layers 1 to 3 green; a layer-4 Ctrl-C and SIGTERM restore run; ceiling recorded | OPS-2, OPS-11, PRI-2, ADR-001, the new ADR row, the size line, `kick-off.md:513`, the tree |
| C59b | The other pieces it needs, `paid`, `proxy/`, `host/` (exec, http, `.env`, override, TLS state, guards), the warnings table with the codes Add providers can raise; ports `provider` as Add providers in both modes; the `--until-stdin-closes` flag on `targets verify` with its CLI and server test | Layers 1 to 3; live: three providers, free checks only (0 paid) | — |
| C60 | Make and Change an alias, Change a provider, families, fallbacks, prices asked of the proxy; ports `alias` | Layers 2, 3; live: an alias with a cross-provider fallback, and a cancel during a check (6 paid) | — |
| C61 | Give an app or agent its key, Change a key, home, rules; ports `key` | Layers 2, 3; live: a key and its try (3 paid) | — |
| C62 | First run with the Machine step, resume at the missing step; ports `install`; bash steps removed | Layers 2 to 4; live: a first run and an interrupted one (4 paid) | AC-54 |
| C63 | Cap spending, See usage and cost, Send traces; ports `budget`, `telemetry` | Layer 2; live: a budget per alias and per key (3 paid) | — |
| C64 | Open to the network, Upgrade (and Rebuild), Back up or restore, See logs, Stop or start, Check everything, Uninstall; ports `remote`, `upgrade`; `setup.sh` becomes the bootstrap only | Layers 2 to 4; `make setup-test` upgrade and restore | — |
| C65 | README rewritten around the console (F1 fixed), images from the frames | `TestQuickStart`; PUB-2 | Phases and features tables |

Each step: implemented, reviewed by Fable with ultracode, at most two fix rounds, gates green, its own commit, as in phase 6. Each piece lands in the step of its first user.

## 12. Dependency cost (measured 2026-11-18)

A probe console with `charm.land/bubbletea/v2` v2.1.2, `charm.land/bubbles/v2` v2.2.1 and `charm.land/lipgloss/v2` v2.1.1, built with the project's Go image: 3 direct and 17 transitive modules (one, `golang.org/x/sys`, already present), all MIT or BSD; the stripped static binary is 4.2 MB against 1.5 MB for a hello world (+2.7 MB). Writing the pick here drops `sahilm/fuzzy`; writing the input here may drop Bubbles and `atotto/clipboard`; C59a measures both and the layer-4 harness's modules. Risks accepted: frequent releases of a recent major version; `github.com/charmbracelet/ultraviolet` pinned at a pseudo-version. If C59a finds detection at `init` time in a library, it is recorded here as an accepted risk or the writer that does it is dropped. `govulncheck` covers the console; a pinned version moves only for a finding or a needed fix. None of it reaches the proxy binary or its image (§5).

## 13. Left to the builder

- Key bindings beyond ↑↓, Enter, Esc, typing to filter and Ctrl-C.
- The colour palette and symbols, within §3's terminal rule.
- The family table's first content (vendors in the presets: OpenAI, Anthropic, Google, xAI, Mistral, DeepSeek, Qwen, Moonshot, MiniMax, Z.ai, Meta, local) and the non-chat patterns.
- The terminal-emulator tooling for layer 4, test-only, chosen in C59a.
- The exact spelling of the new flags of §4.4.

## 14. Kick-off amendments

Applied with the commit of the step named in §11's last column, after review, with exact wording drafted in C59a and approved by the owner:

- OPS-2 (`kick-off.md:552`) and OPS-11 (`kick-off.md:561`) name `setup.sh` as the only program that asks; both become "`setup.sh` and the console it starts". OPS-11 adds: the console never reads or writes the clipboard, never publishes the admin listener, shows a client key only until the reader confirms storing it, and follows logs only while the reader watches. Its "never removes a volume" stays.
- PRI-2 (`kick-off.md:149`) and `kick-off.md:52` reworded to "the proxy has no UI; the console is a separate host binary that configures it through the CLI".
- ADR-001 (`kick-off.md:674`) and the distribution line (`kick-off.md:696`) name the second binary; a new ADR row records the console, its three direct modules and what was discarded (§3).
- The vision's and §23's "at most 20,000 lines of production code" (`kick-off.md:53`, `:902`) apply to the proxy; the console has its own ceiling (§9).
- `kick-off.md:513` and the repository tree (`kick-off.md:1336-1367`) gain the console beside `setup.sh`.
- `targets verify --until-stdin-closes` in the CLI reference.
- AC-54 (`kick-off.md:856`) names the console's flags mode beside `./setup.sh`.
- A row for phase 7 in the phases table; the `setup-assistant` feature row (`kick-off.md:780`) extended to the console.
