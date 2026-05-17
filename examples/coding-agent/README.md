# Autonomous AI coding agent — OpenAI + Vajra

A working autonomous coding agent in **under 250 lines of Python**. An OpenAI
model is given a coding task and three tools that run inside a fresh,
hardware-isolated [Vajra](../../README.md) microVM sandbox. It plans, writes
code, runs it, reads back the results, fixes its own mistakes, and reports
when done.

```
[ agent.py ] --> [ OpenAI API ] --> [ Vajra SDK ] --> [ sandbox /workspace ]
      ^________________________________________________________|
                   tool results feed back to the model
```

This is the canonical pattern for AI agents that execute untrusted code:
the model never touches your machine — every command and file write lands
inside a disposable VM that is destroyed when the run ends.

## The tools

The model drives the sandbox through three tools, each backed by the Vajra SDK:

| Tool           | Backed by                     | What it does                      |
| -------------- | ----------------------------- | --------------------------------- |
| `run_command`  | `client.sandbox.exec`         | Run a shell command, observe I/O  |
| `write_file`   | `client.sandbox.upload_bytes` | Write a file into `/workspace`    |
| `read_file`    | `client.sandbox.download_file`| Read a file back out              |

The agent loop is plain OpenAI tool calling: call the model, run whatever
tools it asks for inside the sandbox, feed the results back as `tool`
messages, repeat until the model stops calling tools and returns a summary.

## Quick start

```sh
cd examples/coding-agent
pip install -r requirements.txt          # openai + the vajra SDK from this repo

export OPENAI_API_KEY=sk-...
export VAJRA_API_KEY=vj_live_...
export VAJRA_API_URL=http://localhost:8080   # your vajra-master endpoint

python agent.py
```

Pass your own task as an argument; otherwise a default FizzBuzz task runs:

```sh
python agent.py "Write a Python script that finds the first 10 prime numbers, run it, and show the output."
```

### Configuration

All settings come from the environment:

| Variable          | Default                  | Purpose                           |
| ----------------- | ------------------------ | --------------------------------- |
| `OPENAI_API_KEY`  | _(required)_             | OpenAI API key                    |
| `VAJRA_API_KEY`   | _(required)_             | Vajra API key (`vj_live_...`)     |
| `VAJRA_API_URL`   | `http://localhost:8080`  | vajra-master base URL             |
| `OPENAI_MODEL`    | `gpt-4o`                 | OpenAI model to drive the agent   |
| `VAJRA_TEMPLATE`  | `ubuntu-noble`           | Sandbox template                  |

## Example run

```
task: Write a Python program to /workspace/fizzbuzz.py ...

creating sandbox from template 'ubuntu-noble' ...
  sandbox sbx_a1b2c3 ready in 142 ms

[model] I'll write the FizzBuzz program, then run it to verify.

[tool] write_file(path=/workspace/fizzbuzz.py, content=for i in range(1, 21): ...)
    wrote 168 bytes to /workspace/fizzbuzz.py

[tool] run_command(command=python3 /workspace/fizzbuzz.py)
    exit_code=0
    1
    2
    Fizz
    ...

[model] Done. fizzbuzz.py prints the correct sequence for 1-20.
--- done in 3 turn(s) ---

destroying sandbox sbx_a1b2c3 ...
  destroyed.
tokens: 4821 prompt + 612 completion = 5433 total
```

## Why this matters

- **Real isolation** — each sandbox is a hardware-isolated Cloud Hypervisor
  microVM, not a container.
- **Sub-200ms boot** — agents don't wait around for cold container starts.
- **Persistent `/workspace`** — files survive across every `exec` call.
- **Snapshot/restore** — an agent can pause, snapshot its state, and resume later.
- **API + SDK** — 3-line setup, no infrastructure to manage.

## Limitations (honest)

- Sandboxes are vsock-only (no internet from inside) — use local data, or
  stage files in with the files API rather than fetching URLs from the guest.
- Token tracking is approximate: it sums the usage fields from each response.
- The agent stops after `MAX_TURNS` (16) round trips as a safety cap.
