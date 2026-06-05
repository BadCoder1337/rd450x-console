"""Runtime configuration and credential loading.

Credentials are read from the environment / a ``.env`` file **only at runtime**.
The password is never logged or echoed; callers reference it by attribute only.
"""

from __future__ import annotations

import os
from dataclasses import dataclass
from pathlib import Path

# Defaults extracted from jviewer.jnlp (the RD450X BMC).
DEFAULT_HOST = "192.168.1.90"
DEFAULT_PORT = 623  # IPMI RMCP+ (UDP), not the JViewer KVM port 7582.


def _project_root() -> Path:
    # src/rd450x_console/config.py -> repo root is three parents up.
    return Path(__file__).resolve().parents[2]


def load_dotenv(path: Path | None = None) -> None:
    """Populate ``os.environ`` from a .env file without printing its contents.

    Existing environment variables win, so a real shell export overrides .env.
    Uses python-dotenv if installed, otherwise a minimal built-in parser.
    """
    env_path = path or (_project_root() / ".env")
    if not env_path.exists():
        return
    try:
        from dotenv import load_dotenv as _ld  # type: ignore

        _ld(dotenv_path=str(env_path), override=False)
        return
    except ModuleNotFoundError:
        pass

    for raw in env_path.read_text(encoding="utf-8").splitlines():
        line = raw.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, _, value = line.partition("=")
        key = key.strip()
        value = value.strip().strip('"').strip("'")
        os.environ.setdefault(key, value)


@dataclass
class Config:
    host: str
    port: int
    user: str
    password: str

    def redacted(self) -> str:
        return f"Config(host={self.host}, port={self.port}, user={self.user!r}, password=***)"


class ConfigError(RuntimeError):
    pass


def load_config(
    host: str | None = None,
    port: int | None = None,
    user: str | None = None,
    env_path: Path | None = None,
) -> Config:
    """Build a Config from CLI overrides, then env vars, then defaults.

    The password is taken exclusively from ``IPMI_PASSWORD`` so it never has to
    appear on a command line.
    """
    load_dotenv(env_path)

    resolved_host = host or os.environ.get("IPMI_HOST") or DEFAULT_HOST
    resolved_port = port or int(os.environ.get("IPMI_PORT", DEFAULT_PORT))
    resolved_user = user or os.environ.get("IPMI_USER")
    password = os.environ.get("IPMI_PASSWORD")

    missing = []
    if not resolved_user:
        missing.append("IPMI_USER")
    if not password:
        missing.append("IPMI_PASSWORD")
    if missing:
        raise ConfigError(
            "Missing credential(s): "
            + ", ".join(missing)
            + ".\nCopy .env.example to .env and fill it in, or export the vars."
        )

    return Config(
        host=resolved_host,
        port=resolved_port,
        user=resolved_user,  # type: ignore[arg-type]
        password=password,  # type: ignore[arg-type]
    )
