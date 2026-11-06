"""Register the gateway chat model used by every NeMo config.

The gateway sends the policy's virtual-key token, its provider/model, and the
process-local guardrail secret on each check. This model copies those onto the call
back into the gateway, so the model call is billed and authorized as the key chosen
on the policy and uses the model chosen there. Configs only name the engine.
"""

import os

import httpx
from langchain_openai import ChatOpenAI

from nemoguardrails.llm.providers import register_chat_provider
from nemoguardrails.server.api import api_request_headers

GATEWAY_URL = os.environ.get("M360_GATEWAY_URL", "http://host.docker.internal:8082").rstrip("/")


def _header(name):
    headers = api_request_headers.get(None)
    if headers is None:
        return ""
    value = headers.get(name)
    return value.strip() if isinstance(value, str) else ""


def _inject(request):
    token = _header("x-m360-llm-api-key")
    secret = _header("x-m360-guardrail-internal")
    if token:
        request.headers["Authorization"] = "Bearer " + token
    if secret:
        request.headers["x-m360-guardrail-internal"] = secret


async def _inject_async(request):
    _inject(request)


class GatewayChatOpenAI(ChatOpenAI):
    def __init__(self, *args, **kwargs):
        kwargs.pop("api_key", None)
        kwargs.pop("default_headers", None)
        kwargs.pop("base_url", None)
        kwargs["api_key"] = "unset"
        kwargs["openai_api_base"] = GATEWAY_URL + "/v1"
        kwargs.setdefault("model", "policy-model")
        kwargs.setdefault("http_client", httpx.Client(event_hooks={"request": [_inject]}))
        kwargs.setdefault("http_async_client", httpx.AsyncClient(event_hooks={"request": [_inject_async]}))
        super().__init__(*args, **kwargs)

    def _get_request_payload(self, input_, *, stop=None, **kwargs):
        payload = super()._get_request_payload(input_, stop=stop, **kwargs)
        model = _header("x-m360-llm-model")
        if model:
            payload["model"] = model
        return payload


def init(app):
    register_chat_provider("m360_gateway", GatewayChatOpenAI)
