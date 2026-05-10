# vajra (Python SDK)

Synchronous Python client for the [Vajra](https://github.com/allenabraham999/vajra)
AI sandbox cloud platform.

## Install

```bash
pip install -e sdk/python
```

## Quick start

```python
from vajra import VajraClient

client = VajraClient(api_key="vj_live_...", base_url="http://localhost:8080")

# Create
sandbox = client.sandbox.create(
    name="test",
    template="ubuntu-noble",
    vcpus=2,
    memory_mb=512,
)

# Run a command
res = client.sandbox.exec(sandbox.id, "echo hello")
print(res.stdout)         # 'hello\n'
print(res.exit_code)      # 0

# Lifecycle
client.sandbox.stop(sandbox.id)
client.sandbox.start(sandbox.id)
client.sandbox.destroy(sandbox.id)

# Snapshots
snap = client.snapshot.create(sandbox.id, name="my-snap")
for s in client.snapshot.list(sandbox.id):
    print(s.id, s.size_bytes)

# Templates
for t in client.template.list():
    print(t.id, t.name, t.version)
```

## Auth

Pass either an API key (`api_key="vj_live_..."`) or a JWT
(`jwt="..."`). API keys are long-lived; JWTs expire after 1h.

## Files

```python
client.sandbox.upload_file(sandbox.id, "/local.txt", "/remote.txt")
client.sandbox.download_file(sandbox.id, "/remote.txt", "/local-copy.txt")
for entry in client.sandbox.list_files(sandbox.id, "/"):
    print(entry.name, entry.size, entry.is_dir)
```

## Errors

Every method raises `vajra.VajraAPIError` on a non-2xx response. The
exception carries `.status` (HTTP code) and `.message` (the master's
error string).
