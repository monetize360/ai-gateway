#!/usr/bin/env python3
"""
Integration tests for tenant-store Bifrost governance:

  1. Routing rule — decoy provider fails without rule, succeeds when routed to fakellm-openai
  2. Rate limit — VK request cap returns HTTP 429
  3. Budget — VK spend cap returns HTTP 402
"""

from __future__ import annotations

import argparse
import asyncio
import sys
import time
from datetime import datetime, timezone
from typing import Optional

try:
    import aiohttp
except ImportError:
    print("aiohttp required: pip install -r scripts/requirements.txt")
    raise SystemExit(1)

from tenant_env import (
    LOAD_TEST_BUDGET_ID,
    LOAD_TEST_PROVIDER_NAME,
    LOAD_TEST_RATE_LIMIT_ID,
    LOAD_TEST_ROUTING_RULE_ID,
    LOAD_TEST_VK_ID,
    first_tenant,
    first_tenant_user_id,
    load_tenant_store_config,
    load_tenant_auth_token,
    wait_for_governance_sync,
)


DECOY_MODEL = "openai/gpt-4o"
ROUTED_MODEL = f"{LOAD_TEST_PROVIDER_NAME}/gpt-4o-mini"
BUDGET_RESET_DURATION = "1h"
RATE_LIMIT_RESET_DURATION = "1h"


async def put_virtual_key(
    session: aiohttp.ClientSession,
    bifrost_url: str,
    token: str,
    vk_id: str,
    body: dict,
) -> tuple[int, str]:
    """Update VK via Bifrost governance API (reloads in-memory governance state)."""
    headers = {"Authorization": f"Bearer {token}", "Content-Type": "application/json"}
    async with session.put(
        f"{bifrost_url}/api/governance/virtual-keys/{vk_id}",
        json=body,
        headers=headers,
        allow_redirects=False,
        timeout=aiohttp.ClientTimeout(total=60),
    ) as resp:
        text = await resp.text()
        return resp.status, text


async def post_chat(
    session: aiohttp.ClientSession,
    bifrost_url: str,
    token: str,
    model: str,
) -> tuple[int, Optional[dict], str]:
    headers = {"Authorization": f"Bearer {token}", "Content-Type": "application/json"}
    payload = {"model": model, "messages": [{"role": "user", "content": "ping"}]}
    try:
        async with session.post(
            f"{bifrost_url}/v1/chat/completions",
            json=payload,
            headers=headers,
            allow_redirects=False,
            timeout=aiohttp.ClientTimeout(total=60),
        ) as resp:
            body_text = await resp.text()
            try:
                import json

                body = json.loads(body_text)
            except Exception:
                body = {"raw": body_text}
            return resp.status, body if isinstance(body, dict) else {"raw": body}, body_text
    except Exception as exc:
        return 0, None, str(exc)


def set_routing_enabled(enabled: bool) -> None:
    cfg = load_tenant_store_config()
    tenant = first_tenant(cfg)
    from tenant_env import connect_tenant

    with connect_tenant(cfg, tenant) as conn, conn.cursor() as cur:
        cur.execute(
            """
            UPDATE routing_rules
            SET enabled = %s, updated_at = NOW() AT TIME ZONE 'UTC'
            WHERE id = %s::uuid
            """,
            (enabled, LOAD_TEST_ROUTING_RULE_ID),
        )
        conn.commit()


def set_budget(max_limit: float, current_usage: float) -> None:
    cfg = load_tenant_store_config()
    tenant = first_tenant(cfg)
    from tenant_env import connect_tenant

    with connect_tenant(cfg, tenant) as conn, conn.cursor() as cur:
        cur.execute(
            """
            UPDATE governance_budgets
            SET max_limit = %s,
                current_usage = %s,
                last_reset = NOW() AT TIME ZONE 'UTC',
                updated_at = NOW() AT TIME ZONE 'UTC',
                deleted = false
            WHERE id = %s::uuid
            """,
            (max_limit, current_usage, LOAD_TEST_BUDGET_ID),
        )
        conn.commit()


async def restore_permissive_limits_via_api(
    session: aiohttp.ClientSession,
    bifrost_url: str,
    token: str,
) -> None:
    status, body = await put_virtual_key(
        session,
        bifrost_url,
        token,
        LOAD_TEST_VK_ID,
        {
            "budgets": [
                {
                    "id": LOAD_TEST_BUDGET_ID,
                    "max_limit": 1000.0,
                    "reset_duration": BUDGET_RESET_DURATION,
                }
            ],
            "rate_limits": [
                {
                    "id": LOAD_TEST_RATE_LIMIT_ID,
                    "request_max_limit": 1000,
                    "request_reset_duration": RATE_LIMIT_RESET_DURATION,
                }
            ],
            "reset_budget_usage": True,
        },
    )
    if status != 200:
        raise RuntimeError(f"failed to restore permissive limits: HTTP {status}: {body[:300]}")


async def put_routing_rule(
    session: aiohttp.ClientSession,
    bifrost_url: str,
    token: str,
    rule_id: str,
    body: dict,
) -> tuple[int, str]:
    headers = {"Authorization": f"Bearer {token}", "Content-Type": "application/json"}
    async with session.put(
        f"{bifrost_url}/api/governance/routing-rules/{rule_id}",
        json=body,
        headers=headers,
        allow_redirects=False,
        timeout=aiohttp.ClientTimeout(total=60),
    ) as resp:
        text = await resp.text()
        return resp.status, text


async def test_routing(
    session: aiohttp.ClientSession,
    bifrost_url: str,
    token: str,
    sync_wait: float,
    refresh_interval: int,
) -> tuple[bool, str]:
    print("\n── Routing rule ──")
    await restore_permissive_limits_via_api(session, bifrost_url, token)
    status, body = await put_routing_rule(
        session,
        bifrost_url,
        token,
        LOAD_TEST_ROUTING_RULE_ID,
        {"enabled": False},
    )
    if status != 200:
        return False, f"failed to disable routing rule via API: HTTP {status}: {body[:300]}"

    status_off, _, raw_off = await post_chat(session, bifrost_url, token, DECOY_MODEL)
    if status_off == 200:
        return False, f"expected failure without routing rule, got HTTP 200: {raw_off[:200]}"

    status, body = await put_routing_rule(
        session,
        bifrost_url,
        token,
        LOAD_TEST_ROUTING_RULE_ID,
        {"enabled": True},
    )
    if status != 200:
        return False, f"failed to re-enable routing rule via API: HTTP {status}: {body[:300]}"

    status_on, body_json, raw_on = await post_chat(session, bifrost_url, token, DECOY_MODEL)
    if status_on != 200:
        return False, f"expected HTTP 200 with routing rule, got {status_on}: {raw_on[:300]}"

    if not body_json or "choices" not in body_json:
        return False, f"unexpected body with routing rule: {body_json}"

    model = body_json.get("model", "")
    print(f"  without rule: blocked (HTTP {status_off})")
    print(f"  with rule   : HTTP 200, response model={model!r}")
    return True, "routing rule applied (decoy openai → fakellm-openai)"


async def test_rate_limit(
    session: aiohttp.ClientSession,
    bifrost_url: str,
    token: str,
    sync_wait: float,
    refresh_interval: int,
) -> tuple[bool, str]:
    print("\n── Rate limit ──")
    await restore_permissive_limits_via_api(session, bifrost_url, token)
    status, body = await put_virtual_key(
        session,
        bifrost_url,
        token,
        LOAD_TEST_VK_ID,
        {
            "rate_limits": [
                {
                    "id": LOAD_TEST_RATE_LIMIT_ID,
                    "request_max_limit": 1,
                    "request_reset_duration": RATE_LIMIT_RESET_DURATION,
                }
            ]
        },
    )
    if status != 200:
        return False, f"failed to tighten rate limit via API: HTTP {status}: {body[:300]}"

    status1, _, raw1 = await post_chat(session, bifrost_url, token, ROUTED_MODEL)
    if status1 != 200:
        return False, f"expected first request HTTP 200 under cap=1, got {status1}: {raw1[:300]}"

    status2, _, raw2 = await post_chat(session, bifrost_url, token, ROUTED_MODEL)
    if status2 != 429:
        return False, f"expected HTTP 429 on second request, got {status2}: {raw2[:300]}"

    print("  request 1: HTTP 200")
    print("  request 2: HTTP 429 (rate limit)")
    await restore_permissive_limits_via_api(session, bifrost_url, token)
    return True, "rate limit enforced (429)"


async def test_budget(
    session: aiohttp.ClientSession,
    bifrost_url: str,
    token: str,
    sync_wait: float,
    refresh_interval: int,
) -> tuple[bool, str]:
    print("\n── Budget ──")
    await restore_permissive_limits_via_api(session, bifrost_url, token)
    budget_payload = {
        "budgets": [
            {
                "id": LOAD_TEST_BUDGET_ID,
                "max_limit": 0.01,
                "reset_duration": BUDGET_RESET_DURATION,
            }
        ],
        "reset_budget_usage": True,
    }
    status, body = await put_virtual_key(session, bifrost_url, token, LOAD_TEST_VK_ID, budget_payload)
    if status != 200:
        return False, f"failed to tighten budget via API: HTTP {status}: {body[:300]}"

    for attempt in range(1, 16):
        status, _, raw = await post_chat(session, bifrost_url, token, ROUTED_MODEL)
        if status == 402:
            print(f"  blocked at HTTP 402 on attempt {attempt}")
            await restore_permissive_limits_via_api(session, bifrost_url, token)
            return True, "budget enforced (402)"
        if status != 200:
            return False, f"expected HTTP 200 or 402, got {status} on attempt {attempt}: {raw[:300]}"

    await restore_permissive_limits_via_api(session, bifrost_url, token)
    return False, "budget was not exhausted after 15 requests (restart Bifrost after seed if counters are stale)"


async def run_tests(args: argparse.Namespace) -> bool:
    cfg = load_tenant_store_config()
    tenant = first_tenant(cfg)
    user_id = first_tenant_user_id(cfg, tenant)
    token = load_tenant_auth_token(tenant.tenant_id, LOAD_TEST_VK_ID, user_id=user_id)

    print("=" * 60)
    print("  Tenant-store governance tests")
    print("=" * 60)
    print(f"  Bifrost     : {args.bifrost_url}")
    print(f"  Tenant      : {tenant.tenant_id} ({tenant.db_name})")
    print(f"  Virtual key : {LOAD_TEST_VK_ID}")
    print(f"  Sync wait   : {args.sync_wait}s (refresh={cfg.refresh_interval_seconds}s)")
    print("=" * 60)

    results: list[tuple[str, bool, str]] = []
    connector = aiohttp.TCPConnector(limit=10)
    async with aiohttp.ClientSession(connector=connector) as session:
        await restore_permissive_limits_via_api(session, args.bifrost_url, token)
        await put_routing_rule(
            session,
            args.bifrost_url,
            token,
            LOAD_TEST_ROUTING_RULE_ID,
            {"enabled": True},
        )

        test_cases = (
            ("rate_limit", test_rate_limit),
            ("budget", test_budget),
            ("routing", test_routing),
        )
        for name, test_fn in test_cases:
            ok, detail = await test_fn(
                session,
                args.bifrost_url,
                token,
                args.sync_wait,
                cfg.refresh_interval_seconds,
            )
            results.append((name, ok, detail))
            mark = "PASS" if ok else "FAIL"
            print(f"\n  [{mark}] {name}: {detail}")

    try:
        async with aiohttp.ClientSession() as cleanup_session:
            await restore_permissive_limits_via_api(cleanup_session, args.bifrost_url, token)
    except Exception as exc:
        print(f"\n  [WARN] cleanup restore failed: {exc}")
    set_routing_enabled(True)
    async with aiohttp.ClientSession() as cleanup_session:
        await put_routing_rule(
            cleanup_session,
            args.bifrost_url,
            token,
            LOAD_TEST_ROUTING_RULE_ID,
            {"enabled": True},
        )

    passed = all(ok for _, ok, _ in results)
    print("\n" + "=" * 60)
    print(f"  Result: {'ALL PASSED' if passed else 'FAILURES'} @ {datetime.now(timezone.utc).isoformat()}")
    print("=" * 60)
    return passed


def main() -> int:
    parser = argparse.ArgumentParser(description="Tenant-store governance integration tests")
    parser.add_argument("--bifrost-url", default="http://localhost:8080")
    parser.add_argument(
        "--sync-wait",
        type=float,
        default=0.0,
        help="Seconds to wait after DB changes (default: refresh_interval + 2)",
    )
    args = parser.parse_args()
    if args.sync_wait <= 0:
        cfg = load_tenant_store_config()
        args.sync_wait = float(cfg.refresh_interval_seconds + 2)

    ok = asyncio.run(run_tests(args))
    return 0 if ok else 1


if __name__ == "__main__":
    raise SystemExit(main())
