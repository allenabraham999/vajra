#!/usr/bin/env python3
"""Autonomous AI coding agent — Claude + Vajra.

Claude is handed a coding task and three tools that execute inside a fresh,
hardware-isolated Vajra microVM sandbox: run a shell command, write a file,
and read a file. Claude plans, acts, observes the results, and iterates until
the task is done — then returns a final summary.

    [ agent.py ] -> [ Claude API ] -> [ Vajra SDK ] -> [ sandbox /workspace ]
         ^_______________________________________________________|
                       tool results feed back to Claude

Usage: set ANTHROPIC_API_KEY, VAJRA_API_KEY and VAJRA_API_URL, then run
`python agent.py ["a coding task in plain English"]`. See README.md for details.

The sandbox has no internet access (vsock-only), so the agent works entirely
with local tools and files. The sandbox is always destroyed on exit.
"""

from __future__ import annotations

import os
import sys
import time
import tempfile

from anthropic import Anthropic
from vajra import VajraClient, VajraAPIError


# --- Configuration (all overridable via environment) -----------------------

MODEL = os.environ.get("ANTHROPIC_MODEL", "claude-sonnet-4-6")
VAJRA_URL = os.environ.get("VAJRA_API_URL", "http://localhost:8080")
TEMPLATE = os.environ.get("VAJRA_TEMPLATE", "ubuntu-noble")
MAX_TURNS = 16            # hard cap on agent <-> sandbox round trips
MAX_TOOL_OUTPUT = 4000    # chars; keeps a noisy tool result from flooding context

DEFAULT_TASK = (
    "Write a Python program to /workspace/fizzbuzz.py that prints the FizzBuzz "
    "sequence for the numbers 1 through 20. Run it, then read the file back to "
    "confirm the source is correct. Report the program's output."
)

SYSTEM_PROMPT = """\
You are an autonomous coding agent working inside a fresh Linux microVM
sandbox. The directory /workspace is persistent across commands.

You have three tools, all of which execute inside the sandbox:
  - run_command: run a shell command and observe its stdout/stderr/exit code
  - write_file:  create or overwrite a file with the given contents
  - read_file:   read a file's contents back

The sandbox has no internet access — work only with local tools and files.
Plan, act, and verify your own work. When the task is complete, stop calling
tools and reply with a concise summary of what you did and the final result.
"""


# --- Tool schemas exposed to Claude ----------------------------------------

def _tool(name: str, description: str, **params: str) -> dict:
    """Build an Anthropic tool schema; every parameter is a required string."""
    return {
        "name": name,
        "description": description,
        "input_schema": {
            "type": "object",
            "properties": {k: {"type": "string", "description": v}
                           for k, v in params.items()},
            "required": list(params),
        },
    }


TOOLS = [
    _tool("run_command",
          "Run a shell command inside the sandbox. Returns stdout, stderr "
          "and the exit code.",
          command="Shell command to run."),
    _tool("write_file",
          "Write text to a file in the sandbox, overwriting any existing file.",
          path="Absolute file path.", content="File contents."),
    _tool("read_file",
          "Read a UTF-8 text file from the sandbox.",
          path="Absolute file path."),
]

# One cache breakpoint on the system prompt also caches the tool schemas, so
# every turn after the first re-uses them instead of re-billing those tokens.
CACHED_SYSTEM = [
    {"type": "text", "text": SYSTEM_PROMPT, "cache_control": {"type": "ephemeral"}}
]


# --- Sandbox-backed tool implementations -----------------------------------

class SandboxTools:
    """Runs each Claude tool call inside one Vajra sandbox."""

    def __init__(self, client: VajraClient, sandbox_id: str):
        self._client = client
        self._sid = sandbox_id

    def run_command(self, command: str) -> str:
        res = self._client.sandbox.exec(self._sid, command, timeout_ms=30_000)
        out = res.stdout or ""
        if res.stderr:
            out += ("\n" if out else "") + "[stderr]\n" + res.stderr
        return f"exit_code={res.exit_code}\n{out}".strip()

    def write_file(self, path: str, content: str) -> str:
        self._client.sandbox.upload_bytes(self._sid, content.encode(), path)
        return f"wrote {len(content)} bytes to {path}"

    def read_file(self, path: str) -> str:
        with tempfile.NamedTemporaryFile() as tmp:
            self._client.sandbox.download_file(self._sid, path, tmp.name)
            data = tmp.read()
        try:
            return data.decode()
        except UnicodeDecodeError:
            return f"<{len(data)} bytes of binary data>"

    def dispatch(self, name: str, args: dict) -> str:
        fn = getattr(self, name, None)
        if fn is None:
            raise ValueError(f"unknown tool: {name}")
        return fn(**args)


def wait_until_running(client: VajraClient, sandbox_id: str, timeout: float = 60.0):
    """Poll until the sandbox is RUNNING (pool hits land in well under 1s)."""
    deadline = time.time() + timeout
    while time.time() < deadline:
        state = client.sandbox.get(sandbox_id).state
        if state == "RUNNING":
            return
        if state in ("ERROR", "STOPPED", "DESTROYED"):
            raise RuntimeError(f"sandbox entered {state} before reaching RUNNING")
        time.sleep(0.1)
    raise TimeoutError(f"sandbox not RUNNING after {timeout:.0f}s")


# --- Console helpers -------------------------------------------------------

def _short_args(args: dict) -> str:
    parts = []
    for key, value in args.items():
        text = str(value).replace("\n", " ")
        parts.append(f"{key}={text[:60] + '...' if len(text) > 60 else text}")
    return ", ".join(parts)


def _indent(text: str) -> str:
    lines = text.splitlines() or ["(no output)"]
    return "\n".join("    " + line for line in lines)


# --- Agent loop ------------------------------------------------------------

def run_agent(task: str) -> int:
    anthropic_key = os.environ.get("ANTHROPIC_API_KEY")
    vajra_key = os.environ.get("VAJRA_API_KEY")
    if not anthropic_key:
        sys.exit("error: set ANTHROPIC_API_KEY")
    if not vajra_key:
        sys.exit("error: set VAJRA_API_KEY")

    claude = Anthropic(api_key=anthropic_key)
    vajra = VajraClient(api_key=vajra_key, base_url=VAJRA_URL)
    in_tokens = out_tokens = cached_tokens = 0

    print(f"task: {task}\n")
    print(f"creating sandbox from template '{TEMPLATE}' ...")
    started = time.time()
    sandbox = vajra.sandbox.create(name=f"coding-agent-{int(started)}", template=TEMPLATE)

    try:
        wait_until_running(vajra, sandbox.id)
        print(f"  sandbox {sandbox.id} ready in {(time.time() - started) * 1000:.0f} ms\n")

        tools = SandboxTools(vajra, sandbox.id)
        messages = [{"role": "user", "content": task}]

        for turn in range(1, MAX_TURNS + 1):
            reply = claude.messages.create(
                model=MODEL,
                max_tokens=2048,
                system=CACHED_SYSTEM,
                tools=TOOLS,
                messages=messages,
            )
            usage = reply.usage
            in_tokens += usage.input_tokens
            out_tokens += usage.output_tokens
            cached_tokens += getattr(usage, "cache_read_input_tokens", 0) or 0

            for block in reply.content:
                if block.type == "text" and block.text.strip():
                    print(f"[claude] {block.text.strip()}\n")

            if reply.stop_reason != "tool_use":
                print(f"--- done in {turn} turn(s) ---")
                return 0

            # Echo Claude's turn back, then run every tool call it requested.
            messages.append({"role": "assistant", "content": reply.content})
            results = []
            for block in reply.content:
                if block.type != "tool_use":
                    continue
                print(f"[tool] {block.name}({_short_args(block.input)})")
                try:
                    output = tools.dispatch(block.name, block.input)
                    is_error = False
                except (VajraAPIError, ValueError, TypeError) as exc:
                    output, is_error = f"error: {exc}", True
                if len(output) > MAX_TOOL_OUTPUT:
                    output = output[:MAX_TOOL_OUTPUT] + "\n...[truncated]"
                print(_indent(output) + "\n")
                results.append({
                    "type": "tool_result",
                    "tool_use_id": block.id,
                    "content": output,
                    "is_error": is_error,
                })
            messages.append({"role": "user", "content": results})

        print(f"--- stopped: hit MAX_TURNS ({MAX_TURNS}) ---")
        return 1
    finally:
        print(f"\ndestroying sandbox {sandbox.id} ...")
        try:
            vajra.sandbox.destroy(sandbox.id)
            print("  destroyed.")
        except VajraAPIError as exc:
            print(f"  warning: destroy failed: {exc}")
        print(f"tokens: {in_tokens} in ({cached_tokens} cached) / {out_tokens} out")


def main() -> None:
    task = " ".join(sys.argv[1:]).strip() or DEFAULT_TASK
    sys.exit(run_agent(task))


if __name__ == "__main__":
    main()
