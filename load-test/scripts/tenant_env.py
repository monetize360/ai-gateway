#!/usr/bin/env python3
"""Shared helpers for tenant-store load / governance tests."""

from __future__ import annotations

import json
import os
import re
import time
import uuid
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Any, Optional

try:
    import jwt
except ImportError:
    jwt = None  # type: ignore

try:
    import psycopg2
    import psycopg2.extras
except ImportError:
    psycopg2 = None  # type: ignore

REPO_ROOT = Path(__file__).resolve().parents[2]
VK_TOKEN_PATH = REPO_ROOT / "load-test" / "dev-config" / "vk_token"
MPILOT_API_PROPS = (
    REPO_ROOT.parent / "mpilotv2" / "backend" / "api" / "src" / "main" / "resources" / "application.properties"
)

CUSTOM_PROVIDER_TYPE = "2382e8b8-bfb0-4c6d-a273-2e5b55fa13a8"

LOAD_TEST_VK_ID = "fd90ad75-b291-49b5-87dc-2a958d085865"
LOAD_TEST_PROVIDER_NAME = "fakellm-openai"
LOAD_TEST_PROVIDER_ID = "ce153470-0673-4846-a2e9-31b7c61311a5"
LOAD_TEST_MODEL_NAME = "gpt-4o-mini"
LOAD_TEST_MODEL_ID = "2c4880d2-6c5e-47ae-8b3a-0322dbe5d262"
LOAD_TEST_ROUTING_RULE_ID = "f8e7d6c5-b4a3-4291-9f8e-7d6c5b4a3920"
LOAD_TEST_BUDGET_ID = "f8e7d6c5-b4a3-4291-9f8e-7d6c5b4a3921"
LOAD_TEST_RATE_LIMIT_ID = "f8e7d6c5-b4a3-4291-9f8e-7d6c5b4a3922"


@dataclass
class TenantInfo:
    tenant_id: str
    db_name: str
    db_user: str
    db_password: str


@dataclass
class TenantStoreConfig:
    global_host: str
    global_port: str
    global_user: str
    global_password: str
    global_db: str
    jwt_public_key: str
    refresh_interval_seconds: int


def load_config_json(path: Optional[Path] = None) -> dict[str, Any]:
    cfg_path = path or (REPO_ROOT / "config.json")
    with open(cfg_path, encoding="utf-8") as fh:
        return json.load(fh)


def load_tenant_store_config(path: Optional[Path] = None) -> TenantStoreConfig:
    raw = load_config_json(path)
    ts = raw.get("tenant_store") or {}
    if not ts.get("enabled"):
        raise RuntimeError("tenant_store.enabled must be true in config.json")
    global_cfg = ts.get("global") or {}
    return TenantStoreConfig(
        global_host=global_cfg["host"],
        global_port=str(global_cfg["port"]),
        global_user=global_cfg["user"],
        global_password=global_cfg["password"],
        global_db=global_cfg["db_name"],
        jwt_public_key=ts.get("jwt_public_key", ""),
        refresh_interval_seconds=int(ts.get("refresh_interval_seconds") or 10),
    )


def _require_psycopg2() -> None:
    if psycopg2 is None:
        raise RuntimeError("psycopg2 is required. Install: pip install -r scripts/requirements.txt")


def connect_global(cfg: TenantStoreConfig):
    _require_psycopg2()
    return psycopg2.connect(
        host=cfg.global_host,
        port=cfg.global_port,
        user=cfg.global_user,
        password=cfg.global_password,
        dbname=cfg.global_db,
    )


def connect_tenant(cfg: TenantStoreConfig, tenant: TenantInfo):
    _require_psycopg2()
    return psycopg2.connect(
        host=cfg.global_host,
        port=cfg.global_port,
        user=tenant.db_user,
        password=tenant.db_password,
        dbname=tenant.db_name,
    )


def db_name_from_jdbc_url(db_url: str) -> str:
    db_url = db_url.strip()
    if "://" not in db_url:
        raise ValueError(f"invalid db_url: {db_url!r}")
    path = db_url.split("://", 1)[1]
    path = path.split("/", 1)[1]
    return path.split("?", 1)[0]


def first_tenant_user_id(cfg: TenantStoreConfig, tenant: TenantInfo) -> str:
    """Return an active users.id for JWT audit columns (updated_by FK)."""
    with connect_tenant(cfg, tenant) as conn, conn.cursor() as cur:
        cur.execute(
            """
            SELECT id::text
            FROM users
            WHERE deleted = false
            ORDER BY created_at
            LIMIT 1
            """
        )
        row = cur.fetchone()
    if not row:
        raise RuntimeError(f"no active users in tenant DB {tenant.db_name}")
    return row[0]


def first_tenant(cfg: TenantStoreConfig) -> TenantInfo:
    with connect_global(cfg) as conn, conn.cursor() as cur:
        cur.execute(
            """
            SELECT id::text, db_url, db_username, db_password
            FROM tenants
            WHERE deleted = false
            ORDER BY created_at
            LIMIT 1
            """
        )
        row = cur.fetchone()
    if not row:
        raise RuntimeError("no tenants found in global DB")
    tenant_id, db_url, db_user, db_password = row
    return TenantInfo(
        tenant_id=tenant_id,
        db_name=db_name_from_jdbc_url(db_url),
        db_user=db_user,
        db_password=db_password,
    )


def load_virtual_key_private_key() -> str:
    env_key = os.environ.get("JWT_VIRTUAL_KEY_PRIVATE", "").strip()
    if env_key:
        return re.sub(r"\s+", "", env_key)
    if not MPILOT_API_PROPS.is_file():
        raise RuntimeError(
            f"JWT private key not found. Set JWT_VIRTUAL_KEY_PRIVATE or ensure {MPILOT_API_PROPS} exists."
        )
    text = MPILOT_API_PROPS.read_text(encoding="utf-8")
    match = re.search(r"^jwt\.virtual-key\.private=(.+)$", text, re.MULTILINE)
    if not match:
        raise RuntimeError("jwt.virtual-key.private missing from mpilotv2 application.properties")
    return re.sub(r"\s+", "", match.group(1))


def _b64_private_key_to_pem(raw_b64: str) -> bytes:
    import base64

    der = base64.b64decode(raw_b64)
    b64 = base64.b64encode(der).decode("ascii")
    lines = "\n".join(b64[i : i + 64] for i in range(0, len(b64), 64))
    return f"-----BEGIN PRIVATE KEY-----\n{lines}\n-----END PRIVATE KEY-----\n".encode()


def _read_vk_token_file(path: Path = VK_TOKEN_PATH) -> Optional[str]:
    if not path.is_file():
        return None
    for line in path.read_text(encoding="utf-8").splitlines():
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        return line.removeprefix("Bearer ").strip()
    return None


def load_tenant_auth_token(
    tenant_id: str,
    virtual_key_id: str,
    *,
    user_id: Optional[str] = None,
    token_path: Optional[Path] = None,
) -> str:
    """Return tenant JWT for Bifrost Authorization header.

    Resolution order: TENANT_VK_JWT env → load-test/dev-config/vk_token → mint fresh JWT.
    """
    env_token = os.environ.get("TENANT_VK_JWT", "").strip()
    if env_token:
        return env_token.removeprefix("Bearer ").strip()
    file_token = _read_vk_token_file(token_path or VK_TOKEN_PATH)
    if file_token:
        return file_token
    return mint_tenant_jwt(tenant_id, virtual_key_id, user_id=user_id)


def mint_tenant_jwt(
    tenant_id: str,
    virtual_key_id: str,
    *,
    user_id: Optional[str] = None,
    morg_id: Optional[str] = None,
    ttl_hours: int = 24,
) -> str:
    if jwt is None:
        raise RuntimeError("PyJWT is required. Install: pip install -r scripts/requirements.txt")
    private_raw = load_virtual_key_private_key()
    private_pem = _b64_private_key_to_pem(private_raw)
    now = datetime.now(timezone.utc)
    payload = {
        "tenantId": tenant_id,
        "userId": user_id or str(uuid.uuid4()),
        "morgId": morg_id or str(uuid.uuid4()),
        "virtualKey": virtual_key_id,
        "roles": ["TENANTADMIN"],
        "iat": now,
        "exp": now + timedelta(hours=ttl_hours),
    }
    return jwt.encode(payload, private_pem, algorithm="RS256")


def wait_for_governance_sync(seconds: float, refresh_interval: int) -> None:
    delay = max(seconds, refresh_interval + 2)
    print(f"  Waiting {delay:.0f}s for tenant governance sync…")
    time.sleep(delay)


def execute_sql(conn, sql: str, params: Optional[tuple] = None) -> None:
    with conn.cursor() as cur:
        cur.execute(sql, params)
    conn.commit()
