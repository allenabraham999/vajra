"""Python SDK for the Vajra AI sandbox cloud platform.

The primary entry point is ``VajraClient``. Resource namespaces hang off
the client instance (``client.sandbox``, ``client.snapshot``,
``client.template``) and return typed dataclasses defined in
``vajra.models``.
"""

from .client import VajraClient, VajraAPIError
from .models import (
    Sandbox,
    SandboxConfig,
    Snapshot,
    Template,
    Node,
    NodeCapacity,
    NodeUsage,
    ExecResult,
    FileEntry,
    APIKey,
)

__all__ = [
    "VajraClient",
    "VajraAPIError",
    "Sandbox",
    "SandboxConfig",
    "Snapshot",
    "Template",
    "Node",
    "NodeCapacity",
    "NodeUsage",
    "ExecResult",
    "FileEntry",
    "APIKey",
]

__version__ = "0.1.0"
