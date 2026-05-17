#!/usr/bin/env python3
"""Autonomous AI coding agent — OpenAI + Vajra.

An OpenAI model is handed a coding task and three tools that execute inside a
fresh, hardware-isolated Vajra microVM sandbox: run a command, write a file,
read a file. The model plans, acts, observes the results, and iterates until
the task is done — then returns a final summary.

    [ agent.py ] -> [ OpenAI API ] -> [ Vajra SDK ] -> [ sandbox /workspace ]
         ^_______________________________________________________|
                     tool results feed back to the model

Usage: set OPENAI_API_KEY, VAJRA_API_KEY and VAJRA_API_URL, then run
`python agent.py ["a coding task in plain English"]`. See README.md for details.

The sandbox has no internet access (vsock-only), so the agent works entirely
with local tools and files. The sandbox is always destroyed on exit.
"""

from __future__ import annotations

import os
import sys
import json
import time
import tempfile

from openai import OpenAI
from vajra import VajraClient, VajraAPIError


# --- Configuration (all overridable via environment) -----------------------

MODEL = os.environ.get("OPENAI_MODEL", "gpt-4o")
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


# --- Tool schemas exposed to the model -------------------------------------

def _tool(name: str, description: str, **params: str) -> dict:
    """Build an OpenAI function-tool schema; every parameter is a required string."""
    return {
        "type": "function",
        "function": {
            "name": name,
            "description": description,
            "parameters": {
                "type": "object",
                "properties": {k: {"type": "string", "description": v}
                               for k, v in params.items()},
                "required": list(params),
            },
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


# --- Sandbox-backed tool implementations -----------------------------------

class SandboxTools:
    """Runs each model tool call inside one Vajra sandbox."""

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
    openai_key = os.environ.get("OPENAI_API_KEY")
    vajra_key = os.environ.get("VAJRA_API_KEY")
    if not openai_key:
        sys.exit("error: set OPENAI_API_KEY")
    if not vajra_key:
        sys.exit("error: set VAJRA_API_KEY")

    openai = OpenAI(api_key=openai_key)
    vajra = VajraClient(api_key=vajra_key, base_url=VAJRA_URL)
    prompt_tokens = completion_tokens = 0

    print(f"task: {task}\n")
    print(f"creating sandbox from template '{TEMPLATE}' ...")
    started = time.time()
    sandbox = vajra.sandbox.create(name=f"coding-agent-{int(started)}", template=TEMPLATE)

    try:
        wait_until_running(vajra, sandbox.id)
        print(f"  sandbox {sandbox.id} ready in {(time.time() - started) * 1000:.0f} ms\n")

        tools = SandboxTools(vajra, sandbox.id)
        messages = [
            {"role": "system", "content": SYSTEM_PROMPT},
            {"role": "user", "content": task},
        ]

        for turn in range(1, MAX_TURNS + 1):
            response = openai.chat.completions.create(
                model=MODEL,
                messages=messages,
                tools=TOOLS,
                tool_choice="auto",
            )
            usage = response.usage
            if usage:
                prompt_tokens += usage.prompt_tokens
                completion_tokens += usage.completion_tokens

            message = response.choices[0].message
            if message.content and message.content.strip():
                print(f"[model] {message.content.strip()}\n")

            messages.append(message)
            if not message.tool_calls:
                print(f"--- done in {turn} turn(s) ---")
                return 0

            # Run every tool call the model requested. OpenAI requires a
            # matching `tool` message for each call before the next request.
            for call in message.tool_calls:
                name = call.function.name
                args: dict = {}
                try:
                    args = json.loads(call.function.arguments or "{}")
                    output = tools.dispatch(name, args)
                except (VajraAPIError, ValueError, TypeError) as exc:
                    output = f"error: {exc}"
                print(f"[tool] {name}({_short_args(args)})")
                if len(output) > MAX_TOOL_OUTPUT:
                    output = output[:MAX_TOOL_OUTPUT] + "\n...[truncated]"
                print(_indent(output) + "\n")
                messages.append({
                    "role": "tool",
                    "tool_call_id": call.id,
                    "content": output,
                })

        print(f"--- stopped: hit MAX_TURNS ({MAX_TURNS}) ---")
        return 1
    finally:
        print(f"\ndestroying sandbox {sandbox.id} ...")
        try:
            vajra.sandbox.destroy(sandbox.id)
            print("  destroyed.")
        except VajraAPIError as exc:
            print(f"  warning: destroy failed: {exc}")
        total = prompt_tokens + completion_tokens
        print(f"tokens: {prompt_tokens} prompt + {completion_tokens} completion "
              f"= {total} total")


def main() -> None:
    task = " ".join(sys.argv[1:]).strip() or DEFAULT_TASK
    sys.exit(run_agent(task))


if __name__ == "__main__":
    main()
