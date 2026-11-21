# Shepherd

A control station for fleets of coding-agent CLIs. You write small **chunks** (work orders) as
markdown files; Shepherd feeds them to whichever agent you have, **verifies** the result itself
(tests, scope, secrets) instead of trusting the model, repairs failures, spreads work across your
quota, and lets you steer it from a terminal dashboard, a web page, or your phone over
[ntfy](https://ntfy.sh).

Works with: **Claude Code, Codex, Gemini CLI, Aider, opencode (and any OpenAI-compatible/local model),
Antigravity (`agy`), Hermes Agent, and Pi.** Pure Go, single binary. Linux and macOS are first-class;
Windows builds and passes unit tests.

---

## 1. Install

```sh
go install github.com/erinwnash/Shepherd/cmd/shepherd@latest
# or from a clone:
git clone https://github.com/erinwnash/Shepherd && cd Shepherd && go build -o shepherd ./cmd/shepherd
```

## 2. Boot it up (5 minutes)

Run these inside the git repo you want agents to work on:

```sh
shepherd doctor      # preflight checks, with a fix for anything that's missing
shepherd init        # detects your language, test commands and installed agent CLIs; writes shepherd.toml
shepherd plan lint   # validates your chunk files (see section 3)
shepherd             # boots the daemon and opens the live dashboard
```

Alternatives to the one-liner above:

| I want to | Command |
|---|---|
| Have an agent survey my repo and write the chunks for me | `shepherd onboard --cli claude --goal "what I want built"` (any of `claude`, `codex`, `gemini`, `aider`, `opencode`, `agy`, `hermes`, `pi`) |
| Start the daemon in the background | `shepherd start` (add `--nocloud` to use only local models) |
| Start paused so I can look first | `shepherd up --preflight` |
| Open the dashboard later | `shepherd dashboard` (`--tab stack`, `--once` for a one-shot snapshot) |
| Browser dashboard with live dependency graph | `shepherd web --port 8080` |
| Run at login (systemd / launchd) | `shepherd service install` |

Per-project state lives in `.shepherd/` (add it to `.gitignore`).

## 3. Writing chunk files

A chunk is one small, verifiable unit of coding work for an agent. Put them under `docs/plan/`
(the default `[queue] root`), one `.md` file each, in any folder layout you like, e.g.
`docs/plan/S01/01A_add-greeting.md`. Files named `epic.md`, `sprint.md`, `retro.md` or `readme.md`
are ignored.

A chunk is **YAML frontmatter** (what the agent may touch, checked by Shepherd) plus a **markdown
body** (the prompt). Copy this template:

````markdown
---
id: 01A
title: Add a greeting helper
sprint: S01
review: LOW            # LOW | MEDIUM | HIGH  (HIGH pauses for your sign-off)
est_effort: S          # S | M | L  (maps to the agent's reasoning effort)
depends_on: []         # ids that must finish first, e.g. ["00Z"]
status: pending        # pending | done | blocked | skipped (Shepherd updates this)
files:                 # the ONLY files this chunk may change
    - new:src/greeting.ts         # "new:" = the chunk creates it
    - src/index.ts                # no prefix = must already exist
    - new:tests/greeting.test.ts
tests:                 # tests that prove it works; run by the verify gate
    - tests/greeting.test.ts
# no_tests_reason: "docs only"   # use INSTEAD of tests: when there truly is nothing to test
# read_only_tests: true          # agent may not edit the listed tests (anti test-tampering)
---
## Goal
One or two sentences: what exists after this chunk that did not before.

## Context (already verified, do not re-investigate)
- Facts the agent needs: function names, file locations, conventions. Saves it wandering.

## Steps
1. Create `src/greeting.ts` exporting `greet(name: string): string` returning `Hello, <name>!`.
2. Call it from `src/index.ts`.
3. Add `tests/greeting.test.ts` covering a normal name and an empty string.

## Out of scope
- Do not touch any other file. No refactors, renames or reformatting.

## Acceptance
- `greet("Ada")` returns `Hello, Ada!` and the new test passes.
- No file outside `files:` changed.
````

Rules of thumb (the linter enforces most of them):

- **Small.** At most 5 files per chunk; bigger ones fail lint as `oversize`. Use
  `shepherd plan split --parts 3 --write` to cut an oversize chunk into sub-chunks.
- **Tests required.** List `tests:` or give a `no_tests_reason`.
- **Declare every file.** Anything the agent changes outside `files:` fails the scope gate and is
  rolled back. Protected paths (`.git`, `.shepherd/`, `shepherd.toml`, CI configs such as `.github/` and similar) can
  never be listed.
- **Required body sections:** `Goal`, `Out of scope`, `Acceptance`. `Steps` and `Context` are
  strongly recommended: the more you tell the agent up front, the fewer retries you burn.
- **Order with `depends_on`.** Dependency cycles and missing ids are lint errors.
- **Want tests written first?** `shepherd plan test-first <id>` turns a chunk into a
  test-authoring chunk plus an implementation chunk that sees the tests read-only.
- Have older "bold-line" work orders? `shepherd plan migrate --from bold-lines`.

Check your work any time:

```sh
shepherd plan lint      # errors and warnings per chunk
shepherd format         # normalise formatting
shepherd queue ls       # what Shepherd sees, in order
```

## 4. Choosing the agent(s)

`shepherd init` writes a tier per agent CLI it finds on your `PATH`. To add or change one by hand,
edit `shepherd.toml`:

```toml
[tiers.hermes]
adapter = "hermes"        # claude | codex | gemini | aider | opencode | agy | hermes | pi
model   = ""              # optional, passed as the agent's own model flag
pool    = "hermes"

[quota.pools.hermes]
mode = "until-rejected"   # keep going until the provider rejects, then back off
backoff_min = [10, 30]

[repair]
chain = ["hermes:2"]      # tier:attempts, tried in order when verification fails
```

How each agent is called (always as an argv list, never through a shell):

| Adapter | Command Shepherd runs |
|---|---|
| `claude` | `claude -p <prompt> ...` |
| `codex` | `codex exec ... -` (prompt on stdin) |
| `gemini` | `gemini -p <prompt>` |
| `aider` | `aider --yes --no-auto-commits --message <prompt>` |
| `opencode` | `opencode run --auto <prompt>` |
| `agy` | `agy -p <prompt> ...` |
| `hermes` | `hermes chat --oneshot -m <model> -q <prompt>` |
| `pi` | `pi -p --model <model> <prompt>` |

Agents run in a sandbox with a scrubbed environment: only the API-key variables that adapter needs
are passed through. Credentials for Hermes/Pi live in their normal config (`~/.hermes`, `~/.pi`) or
in those env vars. Hermes and Pi are non-interactive here, so configure their tool permissions
beforehand; Shepherd's scope gate and rollback are what contain them.

## 5. Driving it from your phone: ntfy

Shepherd sends notifications to an ntfy topic and, while the daemon runs, listens to the same topic
for commands.

**Set up**

1. Install the ntfy app and subscribe to a topic. On public ntfy.sh **the topic name is the
   password**, so use a long random one. The dashboard shows the live topic on the last line of the
   Overview tab (`[1]`).
2. Set it in `shepherd.toml`:

   ```toml
   [notify]
   ntfy_topic     = "my-project-<long-random-string>"   # or ntfy_url for a self-hosted server
   ntfy_token_env = "NTFY_TOKEN"                        # optional: env var holding an access token
   ```
3. `shepherd start`. A commands file is written to `.shepherd/ntfy-commands.toml` with only the
   emergency stop enabled. To enable the rest, copy `examples/ntfy/ntfy-commands.demo.toml` over it,
   rename keywords if you like, and set `enabled = true`.

**Commands to send** (the first word of your message; case-insensitive)

```sh
curl -d "sheep-status" https://ntfy.sh/<your-topic>      # or just type it in the ntfy app
```

| Send | Does |
|---|---|
| `obliviate` | **Full stop.** Kills the running chunk, rolls it back, requeues it, locks the controller until you run `shepherd unlock` on the machine. Always on, cannot be remapped. |
| `sheep-status` | Replies on the topic with state and queue counts |
| `sheep-halt` | Aborts the current chunk, rolls back, requeues; the loop carries on |
| `sheep-pause` / `sheep-resume` | Pause between chunks / continue |
| `sheep-stop` | Clean stop at the next chunk boundary |
| `sheep-start` | Start processing the queue |
| `sheep-drain` / `sheep-undrain` | Spend remaining quota before reset / leave that mode |
| `sheep-hold <id>` / `sheep-release <id>` | Keep a chunk from being scheduled / let it go |
| `sheep-skip <id>` | Skip a chunk |
| `sheep-retry <id>` | Retry a blocked chunk |

`start`, `resume`, `undrain`, `retry` and `release` widen work, so they need
`allow_escalating = true` in the commands file. Never possible over ntfy: `unlock`,
approve/reject, config changes, quota changes.

**Locking it down.** Anyone who learns a public topic can send commands. Press `n` twice in the
dashboard to **rotate** to a fresh random topic without restarting (the old topic gets a final
"retired" message that does not name the new one), put commands on a separate topic with `url = ...`
in the commands file, or use a protected topic with `ntfy_token_env`. Shepherd ignores its own
outbound messages, messages sent before the daemon started, duplicates, and anything over 256 bytes.

## 6. Logs, sessions and day-to-day control

| I want to | Command |
|---|---|
| See what it's doing right now | `shepherd status` |
| See every command and event, including ntfy ones | `shepherd audit --limit 100` (look for `remote.command` and `remote.listening`, actor `ntfy`) |
| Tail the daemon log | `tail -f .shepherd/daemon.log` |
| Live dashboard (the "session" view) | `shepherd dashboard`. Keys: `p` pause, `r` resume, `h` halt, `d` drain, `l` lockdown, `u` unlock, `q` quit |
| Pause / resume / stop at the next boundary | `shepherd pause` / `resume` / `stop` |
| Abort the current chunk now (rolls back, requeues) | `shepherd halt` |
| Emergency stop / release it | `shepherd lockdown` / `shepherd unlock` |
| Spend quota toward reset | `shepherd drain on` / `off` |
| Inspect and steer chunks | `shepherd queue ls`, `queue hold <id>`, `skip`, `retry`, `release`, `unblock`, `pin`, `diff <id>` |
| Review a HIGH-risk chunk | `shepherd queue diff <id>`, then `queue approve <id>` or `queue reject <id>` |
| Unblock everything that got stuck | `shepherd unblock` |
| Quota pools | `shepherd quota show`, `set-floor`, `set-reserve`, `tier`, `reset-bucket` |
| Live config tweaks | `shepherd config show` / `set` / `reload` |
| Sprint PR text / retrospective with cost ledger | `shepherd pr --sprint S01 [--create]`, `shepherd retro` |
| Add a second machine as a worker | `shepherd worker add` on the controller, `shepherd worker join` on the worker (mTLS, LAN) |
| Daemon as a service | `shepherd service install` / `status` / `uninstall` |

Every event is written to a hash-chained audit log, and secrets are redacted before anything is
stored or shown.

## 7. How a chunk runs

1. Shepherd picks the next ready chunk and gives the agent a prompt built from the chunk.
2. The agent works in a git worktree inside a sandbox.
3. Shepherd, not the agent, runs the gates: content-loss, scope (only declared files changed),
   secret scan, then your build/test commands from `shepherd.toml`.
4. On failure it repairs (cheaper tier first, with the failure and earlier attempts handed over),
   and blocks the chunk after the retry budget. On success it commits and moves on.

More detail is in `docs/user-guide.html` (open it in a browser).

## Security

Shepherd never builds shell strings: it spawns argv lists, with no `sh -c`. Agents run in a
sandbox with a scrubbed environment and a path allow-list. If you find a vulnerability, please open
a private security advisory on GitHub instead of a public issue.

## Third-party software

See `THIRD_PARTY_NOTICES.md`. `tools/llamagputop` is a small separate GPU monitor for llama.cpp with
its own LICENSE.

## License

Apache License 2.0. See `LICENSE`. `tools/llamagputop` is MIT-licensed under its own `LICENSE`.
