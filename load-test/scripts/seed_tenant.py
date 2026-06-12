#!/usr/bin/env python3
"""
Seed the first MPilot tenant (from global tenants table) for tenant-store load tests.

- Points fakellm-openai at the local fake-llm container (localhost:8000)
- Activates the load-test virtual key
- Adds routing / budget / rate-limit rows used by governance_test.py
"""

from __future__ import annotations

import argparse
import json
import sys

from tenant_env import (
    CUSTOM_PROVIDER_TYPE,
    LOAD_TEST_BUDGET_ID,
    LOAD_TEST_MODEL_ID,
    LOAD_TEST_MODEL_NAME,
    LOAD_TEST_PROVIDER_ID,
    LOAD_TEST_PROVIDER_NAME,
    LOAD_TEST_RATE_LIMIT_ID,
    LOAD_TEST_ROUTING_RULE_ID,
    LOAD_TEST_VK_ID,
    first_tenant,
    load_tenant_store_config,
)


def seed(fake_llm_url: str, routing_enabled: bool) -> int:
    cfg = load_tenant_store_config()
    tenant = first_tenant(cfg)
    print(f"Seeding tenant {tenant.tenant_id} (db={tenant.db_name})")

    network_config = {
        "base_url": fake_llm_url,
        "default_request_timeout_in_seconds": 30,
        "max_retries": 0,
        "retry_backoff_initial": 500,
        "retry_backoff_max": 5000,
    }
    custom_config = {
        "base_provider_type": "openai",
        "is_key_less": True,
        "allowed_requests": {"list_models": True, "chat_completion": True},
    }

    from tenant_env import connect_tenant

    with connect_tenant(cfg, tenant) as conn:
        with conn.cursor() as cur:
            cur.execute(
                """
                UPDATE config_providers
                SET
                  network_config_json = %s::jsonb,
                  custom_provider_config_json = %s::jsonb,
                  provider_type = %s::uuid,
                  updated_at = NOW() AT TIME ZONE 'UTC',
                  deleted = false
                WHERE id = %s::uuid
                """,
                (
                    json.dumps(network_config),
                    json.dumps(custom_config),
                    CUSTOM_PROVIDER_TYPE,
                    LOAD_TEST_PROVIDER_ID,
                ),
            )
            if cur.rowcount == 0:
                cur.execute(
                    """
                    INSERT INTO config_providers (
                      id, name, network_config_json, custom_provider_config_json,
                      provider_type, deleted
                    ) VALUES (%s::uuid, %s, %s::jsonb, %s::jsonb, %s::uuid, false)
                    ON CONFLICT (id) DO UPDATE SET
                      name = EXCLUDED.name,
                      network_config_json = EXCLUDED.network_config_json,
                      custom_provider_config_json = EXCLUDED.custom_provider_config_json,
                      provider_type = EXCLUDED.provider_type,
                      updated_at = NOW() AT TIME ZONE 'UTC',
                      deleted = false
                    """,
                    (
                        LOAD_TEST_PROVIDER_ID,
                        LOAD_TEST_PROVIDER_NAME,
                        json.dumps(network_config),
                        json.dumps(custom_config),
                        CUSTOM_PROVIDER_TYPE,
                    ),
                )

            cur.execute(
                """
                UPDATE config_providers
                SET provider_type = %s::uuid
                WHERE provider_type IS NULL
                  AND custom_provider_config_json IS NOT NULL
                  AND TRIM(custom_provider_config_json::text) NOT IN ('', '{}')
                """,
                (CUSTOM_PROVIDER_TYPE,),
            )

            cur.execute(
                """
                UPDATE governance_virtual_keys
                SET
                  is_active = true,
                  deleted = false,
                  value = COALESCE(NULLIF(value, ''), 'load-test-vk-' || id::text),
                  encryption_status = COALESCE(encryption_status, 'plain_text')
                WHERE id = %s::uuid
                """,
                (LOAD_TEST_VK_ID,),
            )

            cur.execute(
                """
                INSERT INTO routing_rules (
                  id, name, description, enabled, cel_expression,
                  provider, model, provider_id, model_id,
                  priority, chain_rule, deleted
                ) VALUES (
                  %s::uuid, 'load-test-route-to-fakellm', 'Route decoy openai requests to local fake-llm',
                  %s, 'true',
                  %s, %s, %s::uuid, %s::uuid,
                  0, false, false
                )
                ON CONFLICT (id) DO UPDATE SET
                  enabled = EXCLUDED.enabled,
                  cel_expression = EXCLUDED.cel_expression,
                  provider = EXCLUDED.provider,
                  model = EXCLUDED.model,
                  provider_id = EXCLUDED.provider_id,
                  model_id = EXCLUDED.model_id,
                  priority = EXCLUDED.priority,
                  updated_at = NOW() AT TIME ZONE 'UTC',
                  deleted = false
                """,
                (
                    LOAD_TEST_ROUTING_RULE_ID,
                    routing_enabled,
                    LOAD_TEST_PROVIDER_NAME,
                    LOAD_TEST_MODEL_NAME,
                    LOAD_TEST_PROVIDER_ID,
                    LOAD_TEST_MODEL_ID,
                ),
            )

            cur.execute(
                """
                INSERT INTO governance_budgets (
                  id, max_limit, reset_duration, last_reset, current_usage,
                  virtual_key_id, deleted
                ) VALUES (
                  %s::uuid, 1000.0, '1h', NOW() AT TIME ZONE 'UTC', 0,
                  %s::uuid, false
                )
                ON CONFLICT (id) DO UPDATE SET
                  max_limit = EXCLUDED.max_limit,
                  reset_duration = EXCLUDED.reset_duration,
                  current_usage = EXCLUDED.current_usage,
                  virtual_key_id = EXCLUDED.virtual_key_id,
                  updated_at = NOW() AT TIME ZONE 'UTC',
                  deleted = false
                """,
                (LOAD_TEST_BUDGET_ID, LOAD_TEST_VK_ID),
            )

            cur.execute(
                """
                INSERT INTO governance_rate_limits (
                  id, request_max_limit, request_reset_duration,
                  request_current_usage, request_last_reset,
                  token_max_limit, token_reset_duration,
                  token_current_usage, token_last_reset,
                  virtual_key_id, deleted
                ) VALUES (
                  %s::uuid, 1000, '1h', 0, NOW() AT TIME ZONE 'UTC',
                  NULL, NULL, 0, NOW() AT TIME ZONE 'UTC',
                  %s::uuid, false
                )
                ON CONFLICT (id) DO UPDATE SET
                  request_max_limit = EXCLUDED.request_max_limit,
                  request_reset_duration = EXCLUDED.request_reset_duration,
                  request_current_usage = EXCLUDED.request_current_usage,
                  request_last_reset = EXCLUDED.request_last_reset,
                  virtual_key_id = EXCLUDED.virtual_key_id,
                  updated_at = NOW() AT TIME ZONE 'UTC',
                  deleted = false
                """,
                (LOAD_TEST_RATE_LIMIT_ID, LOAD_TEST_VK_ID),
            )
        conn.commit()

    print("  fakellm-openai →", fake_llm_url)
    print("  virtual key    →", LOAD_TEST_VK_ID, "(active)")
    print("  routing rule   →", LOAD_TEST_ROUTING_RULE_ID, f"(enabled={routing_enabled})")
    print("  budget / RL    → permissive defaults (tests tighten per scenario)")
    return 0


def main() -> int:
    parser = argparse.ArgumentParser(description="Seed first tenant for load-test governance")
    parser.add_argument("--fake-llm-url", default="http://localhost:8000/")
    parser.add_argument("--disable-routing", action="store_true")
    args = parser.parse_args()
    try:
        return seed(args.fake_llm_url, routing_enabled=not args.disable_routing)
    except Exception as exc:
        print(f"ERROR: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
