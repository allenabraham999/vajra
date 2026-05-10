"""Synchronous HTTP client for vajra-master.

The wire protocol is documented in ``internal/master/`` in the Go repo;
this module mirrors that surface field-for-field. All resource
namespaces (``client.sandbox``, ``client.snapshot``, ``client.template``)
delegate to the same underlying ``_request`` so authentication,
timeouts, and error handling live in one place.
"""

from __future__ import annotations

import os
from typing import Any, BinaryIO, Iterable, Optional, Union
from urllib.parse import quote_plus

import requests

from .models import (
    APIKey,
    ExecResult,
    FileEntry,
    Node,
    Sandbox,
    Snapshot,
    Template,
)

# DEFAULT_TIMEOUT bounds a single control-plane HTTP call. Long
# operations (file upload, exec) override this per-call.
DEFAULT_TIMEOUT = 60.0
FILE_TIMEOUT = 30 * 60.0


class VajraAPIError(Exception):
    """Raised when vajra-master returns a non-2xx response."""

    def __init__(self, status: int, message: str):
        super().__init__(f"vajra api error {status}: {message}")
        self.status = status
        self.message = message


class _Resource:
    """Base mixin that lets resource namespaces reuse the parent client."""

    def __init__(self, client: "VajraClient"):
        self._client = client


class _SandboxResource(_Resource):
    """``client.sandbox`` — sandbox CRUD + lifecycle + exec + files."""

    def create(
        self,
        name: str,
        template: Optional[str] = None,
        snapshot: Optional[str] = None,
        vcpus: int = 2,
        memory_mb: int = 512,
        disk_gb: int = 5,
        region: Optional[str] = None,
    ) -> Sandbox:
        """Create a new sandbox.

        Pass exactly one of ``template`` (a template ID) or ``snapshot``
        (a snapshot ID). The master accepts either as the source artifact;
        the SDK encodes the right ``source`` field for you.
        """
        if (template is None) == (snapshot is None):
            raise ValueError("exactly one of template or snapshot must be set")
        body: dict[str, Any] = {
            "name": name,
            "vcpus": vcpus,
            "memory_mb": memory_mb,
            "disk_gb": disk_gb,
        }
        if template is not None:
            body["source"] = "image"
            body["template_id"] = template
        else:
            body["source"] = "snapshot"
            body["snapshot_id"] = snapshot
        if region:
            body["region"] = region
        data = self._client._request("POST", "/v1/sandboxes", json=body)
        return Sandbox.from_dict(data)

    def get(self, sandbox_id: str) -> Sandbox:
        data = self._client._request("GET", f"/v1/sandboxes/{sandbox_id}")
        return Sandbox.from_dict(data)

    def list(self, limit: int = 0, offset: int = 0) -> list[Sandbox]:
        params: dict[str, Any] = {}
        if limit:
            params["limit"] = limit
        if offset:
            params["offset"] = offset
        data = self._client._request("GET", "/v1/sandboxes", params=params)
        return [Sandbox.from_dict(d) for d in (data or [])]

    def exec(self, sandbox_id: str, command: str, timeout_ms: int = 0) -> ExecResult:
        body: dict[str, Any] = {"command": command}
        if timeout_ms > 0:
            body["timeout_ms"] = timeout_ms
        # Master adds 5s of headroom on top of timeout_ms; mirror that
        # here so a long-running guest command doesn't trip the SDK
        # before the master returns.
        wall = (timeout_ms / 1000.0 + 5) if timeout_ms else DEFAULT_TIMEOUT
        data = self._client._request(
            "POST", f"/v1/sandboxes/{sandbox_id}/exec",
            json=body, timeout=wall,
        )
        return ExecResult.from_dict(data)

    def stop(self, sandbox_id: str) -> Sandbox:
        data = self._client._request("POST", f"/v1/sandboxes/{sandbox_id}/stop")
        return Sandbox.from_dict(data)

    def start(self, sandbox_id: str) -> Sandbox:
        data = self._client._request("POST", f"/v1/sandboxes/{sandbox_id}/start")
        return Sandbox.from_dict(data)

    def destroy(self, sandbox_id: str) -> Sandbox:
        data = self._client._request("DELETE", f"/v1/sandboxes/{sandbox_id}")
        return Sandbox.from_dict(data)

    # Archive / migrate ---------------------------------------------------

    def archive(self, sandbox_id: str) -> dict[str, Any]:
        """Stop and compress a sandbox into cold storage.

        Returns the archive descriptor: ``{operation_id, id, path,
        location, size_bytes}``. ``path`` is a filesystem path when
        ``location == "local"`` and an ``s3://...`` URL otherwise.
        Hold onto the path if you might rehydrate onto a different
        host than the original.
        """
        return self._client._request(
            "POST", f"/v1/sandboxes/{sandbox_id}/archive"
        ) or {}

    def rehydrate(
        self,
        sandbox_id: str,
        archive_path: Optional[str] = None,
        node_id: Optional[str] = None,
    ) -> Sandbox:
        """Restore an archived sandbox to STOPPED.

        ``archive_path`` is optional — when omitted the agent resolves
        the archive from its configured store. ``node_id`` overrides
        placement; otherwise the original node is reused if still
        ACTIVE, falling through to the scheduler.
        """
        body: dict[str, Any] = {}
        if archive_path is not None:
            body["archive_path"] = archive_path
        if node_id is not None:
            body["node_id"] = node_id
        data = self._client._request(
            "POST", f"/v1/sandboxes/{sandbox_id}/rehydrate", json=body
        )
        return Sandbox.from_dict(data)

    def migrate(self, sandbox_id: str, target_node_id: str) -> dict[str, Any]:
        """Move a sandbox to another node (admin-only).

        Returns the migration descriptor:
        ``{operation_id, id, source_node_id, target_node_id, bytes_sent}``.
        """
        return self._client._request(
            "POST", f"/v1/sandboxes/{sandbox_id}/migrate",
            json={"target_node_id": target_node_id},
        ) or {}

    # Files ---------------------------------------------------------------

    def upload_file(
        self,
        sandbox_id: str,
        local_path: str,
        remote_path: str,
        mode: Optional[int] = None,
    ) -> None:
        """Upload a local file into the sandbox at ``remote_path``."""
        size = os.path.getsize(local_path)
        if mode is None:
            mode = os.stat(local_path).st_mode & 0o777
        headers = {
            "X-Vajra-Path": remote_path,
            "X-Vajra-Mode": str(mode),
            "Content-Type": "application/octet-stream",
            "Content-Length": str(size),
        }
        with open(local_path, "rb") as f:
            self._client._request(
                "POST", f"/v1/sandboxes/{sandbox_id}/files/upload",
                data=f, headers=headers, timeout=FILE_TIMEOUT,
            )

    def upload_bytes(
        self,
        sandbox_id: str,
        body: Union[bytes, BinaryIO],
        remote_path: str,
        mode: int = 0o644,
    ) -> None:
        """Upload an in-memory blob or open file handle to ``remote_path``."""
        if isinstance(body, (bytes, bytearray)):
            size = len(body)
            payload: Any = bytes(body)
        else:
            cur = body.tell()
            body.seek(0, os.SEEK_END)
            size = body.tell() - cur
            body.seek(cur)
            payload = body
        headers = {
            "X-Vajra-Path": remote_path,
            "X-Vajra-Mode": str(mode),
            "Content-Type": "application/octet-stream",
            "Content-Length": str(size),
        }
        self._client._request(
            "POST", f"/v1/sandboxes/{sandbox_id}/files/upload",
            data=payload, headers=headers, timeout=FILE_TIMEOUT,
        )

    def download_file(
        self, sandbox_id: str, remote_path: str, local_path: str
    ) -> int:
        """Stream a remote file into ``local_path``. Returns bytes written."""
        url = f"/v1/sandboxes/{sandbox_id}/files/download?path={quote_plus(remote_path)}"
        with self._client._stream("GET", url, timeout=FILE_TIMEOUT) as resp:
            written = 0
            with open(local_path, "wb") as out:
                for chunk in resp.iter_content(chunk_size=64 * 1024):
                    if chunk:
                        out.write(chunk)
                        written += len(chunk)
            return written

    def list_files(self, sandbox_id: str, directory: str = "/") -> list[FileEntry]:
        url = f"/v1/sandboxes/{sandbox_id}/files/list?dir={quote_plus(directory)}"
        data = self._client._request("GET", url)
        entries: Iterable[dict[str, Any]] = (data or {}).get("entries") or []
        return [FileEntry.from_dict(e) for e in entries]


class _SnapshotResource(_Resource):
    """``client.snapshot`` — snapshot create/list/restore."""

    def create(self, sandbox_id: str, name: str) -> Snapshot:
        data = self._client._request(
            "POST", f"/v1/sandboxes/{sandbox_id}/snapshot",
            json={"name": name},
        )
        return Snapshot.from_dict(data)

    def list(self, sandbox_id: str) -> list[Snapshot]:
        data = self._client._request("GET", f"/v1/sandboxes/{sandbox_id}/snapshots")
        return [Snapshot.from_dict(d) for d in (data or [])]

    def restore(
        self,
        snapshot_id: str,
        name: str,
        vcpus: int = 2,
        memory_mb: int = 512,
        disk_gb: int = 5,
    ) -> Sandbox:
        data = self._client._request(
            "POST", f"/v1/snapshots/{snapshot_id}/restore",
            json={
                "name": name,
                "vcpus": vcpus,
                "memory_mb": memory_mb,
                "disk_gb": disk_gb,
            },
        )
        return Sandbox.from_dict(data)


class _TemplateResource(_Resource):
    """``client.template`` — template metadata reads."""

    def list(self) -> list[Template]:
        data = self._client._request("GET", "/v1/templates")
        return [Template.from_dict(d) for d in (data or [])]


class _NodeResource(_Resource):
    """``client.node`` — admin-only node listing and drain."""

    def list(self) -> list[Node]:
        data = self._client._request("GET", "/v1/nodes")
        return [Node.from_dict(d) for d in (data or [])]

    def drain(self, node_id: str) -> dict[str, str]:
        return self._client._request("POST", f"/v1/nodes/{node_id}/drain")


class _APIKeyResource(_Resource):
    """``client.api_keys`` — API key CRUD."""

    def create(self, name: str) -> APIKey:
        data = self._client._request("POST", "/v1/api-keys", json={"name": name})
        return APIKey.from_dict(data)

    def list(self) -> list[APIKey]:
        data = self._client._request("GET", "/v1/api-keys")
        return [APIKey.from_dict(d) for d in (data or [])]

    def revoke(self, key_id: str) -> None:
        self._client._request("DELETE", f"/v1/api-keys/{key_id}")


class VajraClient:
    """Top-level vajra-master client.

    Pass either ``api_key`` (long-lived, recommended for automation) or
    ``jwt`` (1h TTL, what ``vajra login`` produces). The two are mutually
    exclusive — if both are set, ``api_key`` wins.
    """

    def __init__(
        self,
        api_key: Optional[str] = None,
        jwt: Optional[str] = None,
        base_url: str = "http://localhost:8080",
        timeout: float = DEFAULT_TIMEOUT,
        session: Optional[requests.Session] = None,
    ):
        if not api_key and not jwt:
            raise ValueError("api_key or jwt is required")
        self.base_url = base_url.rstrip("/")
        self._token = api_key or jwt
        self._timeout = timeout
        self._session = session or requests.Session()

        self.sandbox = _SandboxResource(self)
        self.snapshot = _SnapshotResource(self)
        self.template = _TemplateResource(self)
        self.node = _NodeResource(self)
        self.api_keys = _APIKeyResource(self)

    # Internals -----------------------------------------------------------

    def _headers(self, extra: Optional[dict[str, str]] = None) -> dict[str, str]:
        h = {"Authorization": f"Bearer {self._token}"}
        if extra:
            h.update(extra)
        return h

    def _request(
        self,
        method: str,
        path: str,
        *,
        json: Any = None,
        data: Any = None,
        params: Optional[dict[str, Any]] = None,
        headers: Optional[dict[str, str]] = None,
        timeout: Optional[float] = None,
    ) -> Any:
        url = self.base_url + path
        resp = self._session.request(
            method, url,
            json=json, data=data, params=params,
            headers=self._headers(headers),
            timeout=timeout or self._timeout,
        )
        return self._handle(resp)

    def _stream(
        self,
        method: str,
        path: str,
        *,
        timeout: Optional[float] = None,
    ) -> requests.Response:
        url = self.base_url + path
        resp = self._session.request(
            method, url,
            headers=self._headers(),
            timeout=timeout or self._timeout,
            stream=True,
        )
        if resp.status_code >= 400:
            try:
                self._handle(resp)
            finally:
                resp.close()
        return resp

    @staticmethod
    def _handle(resp: requests.Response) -> Any:
        if resp.status_code >= 400:
            try:
                body = resp.json()
                msg = body.get("error") or resp.text
            except ValueError:
                msg = resp.text or "unknown error"
            raise VajraAPIError(resp.status_code, msg)
        if resp.status_code == 204 or not resp.content:
            return None
        ct = resp.headers.get("Content-Type", "")
        if "application/json" in ct:
            return resp.json()
        return resp.content

    def close(self) -> None:
        self._session.close()

    def __enter__(self) -> "VajraClient":
        return self

    def __exit__(self, exc_type, exc, tb) -> None:
        self.close()
