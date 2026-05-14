"""Dataclass models mirroring the wire shapes from vajra-master.

Every dataclass has a ``from_dict`` constructor that ignores unknown
keys, so a forward-compatible master can add fields without breaking
older SDK installs.
"""

from __future__ import annotations

from dataclasses import dataclass, field, fields
from datetime import datetime
from typing import Any, Optional


def _parse_dt(value: Any) -> Optional[datetime]:
    """Parse the RFC3339 timestamps master emits.

    Returns ``None`` for empty / null values; raises ``ValueError`` for
    anything else that isn't parseable so SDK callers see the bad data
    rather than silently getting ``None``.
    """
    if value in (None, ""):
        return None
    if isinstance(value, datetime):
        return value
    s = value.rstrip("Z")
    # datetime.fromisoformat supports a "+00:00" offset but not "Z".
    if "+" not in s and "-" not in s[10:]:
        s = s + "+00:00"
    return datetime.fromisoformat(s)


def _filter_known(cls, data: dict[str, Any]) -> dict[str, Any]:
    """Strip keys not present on ``cls`` so unknown wire fields don't blow up."""
    known = {f.name for f in fields(cls)}
    return {k: v for k, v in data.items() if k in known}


@dataclass
class SandboxConfig:
    vcpus: int = 0
    memory_mb: int = 0
    disk_gb: int = 0

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "SandboxConfig":
        return cls(**_filter_known(cls, data or {}))


@dataclass
class Sandbox:
    id: str = ""
    name: str = ""
    account_id: str = ""
    node_id: Optional[str] = None
    cluster_id: Optional[str] = None
    template_id: str = ""
    state: str = ""
    config: SandboxConfig = field(default_factory=SandboxConfig)
    auto_stop_minutes: int = 0
    auto_archive_minutes: int = 0
    last_activity: Optional[datetime] = None
    created_at: Optional[datetime] = None
    updated_at: Optional[datetime] = None
    operation_id: str = ""

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "Sandbox":
        d = dict(data or {})
        d["config"] = SandboxConfig.from_dict(d.get("config") or {})
        d["created_at"] = _parse_dt(d.get("created_at"))
        d["updated_at"] = _parse_dt(d.get("updated_at"))
        d["last_activity"] = _parse_dt(d.get("last_activity"))
        return cls(**_filter_known(cls, d))


@dataclass
class Snapshot:
    id: str = ""
    sandbox_id: str = ""
    account_id: str = ""
    node_id: str = ""
    storage_path: str = ""
    size_bytes: int = 0
    created_at: Optional[datetime] = None

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "Snapshot":
        d = dict(data or {})
        d["created_at"] = _parse_dt(d.get("created_at"))
        return cls(**_filter_known(cls, d))


@dataclass
class Template:
    id: str = ""
    account_id: str = ""
    name: str = ""
    version: str = ""
    hash: str = ""
    rootfs_path: str = ""
    kernel_path: str = ""
    snapshot_path: str = ""
    created_at: Optional[datetime] = None

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "Template":
        d = dict(data or {})
        d["created_at"] = _parse_dt(d.get("created_at"))
        return cls(**_filter_known(cls, d))


@dataclass
class NodeCapacity:
    total_cpu: int = 0
    total_memory_mb: int = 0
    total_disk_gb: int = 0

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "NodeCapacity":
        return cls(**_filter_known(cls, data or {}))


@dataclass
class NodeUsage:
    used_cpu: int = 0
    used_memory_mb: int = 0
    used_disk_gb: int = 0

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "NodeUsage":
        return cls(**_filter_known(cls, data or {}))


@dataclass
class Node:
    id: str = ""
    cluster_id: str = ""
    hostname: str = ""
    ip: str = ""
    state: str = ""
    capacity: NodeCapacity = field(default_factory=NodeCapacity)
    used_resources: NodeUsage = field(default_factory=NodeUsage)
    last_heartbeat: Optional[datetime] = None

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "Node":
        d = dict(data or {})
        d["capacity"] = NodeCapacity.from_dict(d.get("capacity") or {})
        d["used_resources"] = NodeUsage.from_dict(d.get("used_resources") or {})
        d["last_heartbeat"] = _parse_dt(d.get("last_heartbeat"))
        return cls(**_filter_known(cls, d))


@dataclass
class ExecResult:
    exit_code: int = 0
    stdout: str = ""
    stderr: str = ""

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "ExecResult":
        return cls(**_filter_known(cls, data or {}))


@dataclass
class FileEntry:
    name: str = ""
    size: int = 0
    mode: int = 0
    is_dir: bool = False
    mod_time: Optional[datetime] = None

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "FileEntry":
        d = dict(data or {})
        d["mod_time"] = _parse_dt(d.get("mod_time"))
        return cls(**_filter_known(cls, d))


@dataclass
class APIKey:
    id: str = ""
    name: str = ""
    created_at: Optional[datetime] = None
    # Only populated on the create response.
    key: str = ""

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "APIKey":
        d = dict(data or {})
        d["created_at"] = _parse_dt(d.get("created_at"))
        return cls(**_filter_known(cls, d))


@dataclass
class Build:
    """Async Dockerfile → Template build job."""

    id: str = ""
    account_id: str = ""
    template_name: str = ""
    template_version: str = ""
    status: str = ""
    template_id: Optional[str] = None
    error: Optional[str] = None
    created_at: Optional[datetime] = None
    completed_at: Optional[datetime] = None

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "Build":
        d = dict(data or {})
        d["created_at"] = _parse_dt(d.get("created_at"))
        d["completed_at"] = _parse_dt(d.get("completed_at"))
        return cls(**_filter_known(cls, d))


@dataclass
class Webhook:
    """Per-account outbound notification target."""

    id: str = ""
    account_id: str = ""
    url: str = ""
    # Only populated on the create response.
    secret: str = ""
    events: list[str] = field(default_factory=list)
    active: bool = True
    created_at: Optional[datetime] = None

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "Webhook":
        d = dict(data or {})
        d["created_at"] = _parse_dt(d.get("created_at"))
        return cls(**_filter_known(cls, d))
