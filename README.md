# qeuro CLI

Terminal client for the Qeuro AI proxy. Classic chat design: a prompt, a
streamed reply, and a short dim usage line after each answer. Info panels
(`/help`, `/usage`, `/context`) print once when you ask for them and scroll
away with the rest of the transcript.

## Build

```sh
go build -o qeuro .
```

Dependencies are limited to the TUI stack (Bubble Tea, Lip Gloss, Glamour) and
`go-keyring` for OS keychain access. Everything else — HTTP, SSE, JSON, TOML
parsing — is standard library.

## Commands

| Command                            | Description                                       |
| ---------------------------------- | ------------------------------------------------- |
| `qeuro`                            | start an interactive chat session                 |
| `qeuro --local`                    | chat through Ollama or a llama.cpp-compatible API |
| `qeuro chat [--budget <credits>]`  | same, with a hard credit ceiling for this session |
| `qeuro fix [--yes] [--local]`      | propose a fix for the last shell command          |
| `qeuro cost [--since 7d] [--json]` | what this account spent, by model and by day      |
| `qeuro resume [id\|list]`          | continue a previous session (newest by default)   |
| `qeuro config doctor`              | every setting and which layer set it              |
| `qeuro mcp <subcommand>`           | inspect MCP servers, or run this CLI as one       |
| `qeuro completion <shell>`         | print a bash, zsh or fish completion script       |
| `qeuro run --headless`             | one prompt, no TUI (entry point for runners)      |
| `qeuro login`                      | store your `qeuro_live_` token                    |
| `qeuro logout`                     | forget the stored token (and revoke it)           |
| `qeuro whoami`                     | plan and remaining credits                        |
| `qeuro star <github-login>`        | +credits for starring the repo                    |
| `qeuro version`                    | print version info                                |

### First launch

Running `qeuro` without a stored CLI token pauses before hooks and the TUI and
asks for one explicit setup path:

1. **Create a Qeuro account** — opens the console registration page.
2. **Connect an AI provider (BYOK)** — opens the console Providers page, with
   the destination preserved through sign-in.

The prompt returns on every signed-out launch, so dismissing a browser tab does
not permanently hide setup. It performs no network probe; a browser opens only
after a choice. `qeuro --local` and a configured `QEURO_LOCAL=1` session already
have a provider path and skip the cloud setup prompt.

### Shell completion

```bash
qeuro completion bash > /etc/bash_completion.d/qeuro   # or eval "$(qeuro completion bash)"
qeuro completion zsh  > "${fpath[1]}/_qeuro"
qeuro completion fish > ~/.config/fish/completions/qeuro.fish
```

The script is generated from the same registry that drives dispatch and `qeuro
help`, so it cannot offer a command this binary does not have. It completes
commands, their aliases, subcommands and flags — but never *values*: completing a
model or session id would mean running the binary on every Tab.

### Spending

`qeuro cost` reports a window of billed calls — totals, a per-model breakdown and a
per-day series — plus the current balance. `--since` takes `7d`, `2w`, `48h` or a
bare day count; the window is aligned to whole UTC days, so two runs an hour apart
give the same buckets. `--json` prints the same numbers for scripts.

`--budget N` sets a hard ceiling in credits for one session. It is checked before
each provider call, including the ones a tool loop makes on its own, so a single
turn cannot bill past it; when the ceiling is reached the turn stops and keeps the
partial answer. The status bar shows how much of the ceiling is left. `0` or no
flag means no ceiling.

It is a guard on your own session, not a server-side limit — the authoritative
limit is your credit balance.

### Local inference

`qeuro --local` (or `qeuro chat --local`) sends inference directly to a local
model server and does not require a Qeuro account. Ollama is used by default:

```bash
ollama serve
ollama pull qwen2.5-coder:7b
qeuro --local
```

The default endpoint is `http://localhost:11434`. Override it for Ollama,
llama.cpp, LM Studio, or another OpenAI-compatible local server:

```bash
qeuro chat --local --local-url http://127.0.0.1:8080 --local-model my-model
qeuro fix --local
```

The same values can be set with `QEURO_LOCAL=1`, `QEURO_LOCAL_URL`, and
`QEURO_LOCAL_MODEL`, or in the user config. They are intentionally rejected in
`./.qeuro.toml`: a cloned repository must not choose where source code is sent.
Omit `local_model` to use the first installed Ollama model; OpenAI-compatible
servers use their already-loaded model.

Local mode never falls back to the Qeuro backend, never sends the backend bearer
token, ignores ambient HTTP proxy variables, and does not auto-start configured
MCP servers. Tool calling and team mode work when the selected local model and
server support function calls. Credit usage is not available because no backend
billing event exists. HTTPS closed-network endpoints are supported, so `local`
means backend-disabled rather than loopback-only.

## Slash commands (inside a session)

`/help` lists every command with its hotkey, and is the source of truth — the
palette and `/help` both read one registry (`internal/commands`). The ones you
reach for most:

| Command           | Description                          |
| ----------------- | ------------------------------------ |
| `/model [name]`   | list models or switch the active one |
| `/effort [level]` | low · medium · high · xhigh          |
| `/approvals`      | auto-approval: ask / edits / all     |
| `/context`        | context window usage                 |
| `/usage`          | session tokens and credits           |
| `/resume [id]`    | restore a previous session           |
| `/sessions`       | recorded sessions and this one's id  |
| `/undo`           | undo the last file edit              |
| `/clear`          | reset history and clear the screen   |
| `/exit`           | exit (`ctrl+c ×2`)                   |

`Ctrl+C` cancels a streaming reply without ending the session, keeping the partial
answer in the conversation and in the session journal. A second `Ctrl+C` quits.
`Esc` also cancels.

## Configuration

Settings resolve in this order, highest first:

1. command-line flags (`--budget`, `--model`, `--local`, `--local-url`,
   `--local-model`)
2. environment — `QEURO_API_URL`, `QEURO_TOKEN`, `QEURO_MODEL`, `QEURO_BUDGET`,
   `QEURO_CONSOLE_URL`, `QEURO_AUTO_APPROVE`, `QEURO_SKILLS_DIR`, `QEURO_LOCAL`,
   `QEURO_LOCAL_URL`, `QEURO_LOCAL_MODEL`, `QEURO_UNSAFE_PARALLEL_WRITES`
3. `./.qeuro.toml` — the current project
4. `~/.qeuro/config.toml` — you
5. `~/.config/qeuro/config.json` (0600) — written by `qeuro login`
6. built-in defaults

`qeuro config doctor` prints every setting, the value in force, the layer that set
it and the exact file and line. Use it instead of guessing: an overridden value
looks identical to one that was never read.

`./.qeuro.toml` arrives with `git clone`, so a project file may set **only**
`model`. Every other key is refused there with a reason — a cloned repository does
not get to redirect your token (`base_url`, `console_url`, `token`), decide whose
account is billed, remove you from the approval loop (`auto_approve`), feed
instructions to the model (`skills_dir`), lift a ceiling on your own money
(`budget`), or choose where prompts are sent (`local`, `local_url`,
`local_model`), or turn on a mode that discards your edits
(`unsafe_parallel_writes`).

### Parallel writers work in separate trees

In a team run with more than one agent working at a time — `qeuro run --parallel N`
with N > 1, or a plan with several subtasks in the TUI — each writer gets its own
working tree. It reads the project normally and its edits go only to its own tree.
When the step finishes, the changes are applied to your project in plan order.

This exists because sharing one tree loses work. Two agents patching the same file
both get back `ok: file … modified` and only one edit survives, with no error and
no conflict — measured at 12 out of 12 runs before isolation. Separate trees make
that impossible, and turn it into something you can see: if two writers changed the
same file, the run reports the path and applies **nothing**, rather than picking a
winner silently.

Two consequences worth knowing:

- Agents in a parallel step cannot run shell commands. A command's effects cannot
  be confined to one writer's tree, so the tester runs the build and tests once,
  afterwards, in your actual project.
- If a conflict is reported, re-run with a plan that gives each writer its own
  files, or with one worker.

`QEURO_UNSAFE_PARALLEL_WRITES=1` skips isolation and puts the writers back in one
shared tree. The run warns that it can lose edits. The flag exists for one release
as an escape hatch and will be removed.

Set `NO_COLOR=1` to disable ANSI colors.
