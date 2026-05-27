"""
Fake OpenAI-compatible LLM inference API.

Returns realistic-looking chat completion responses with randomised token
usage so that Bifrost emits meaningful trace records into the
Kafka observability connector.
"""

import asyncio
import random
import time
import uuid

from fastapi import FastAPI, Request
from fastapi.responses import JSONResponse
from pydantic import BaseModel

app = FastAPI(title="Fake LLM API", version="1.0.0")

FAKE_RESPONSES = [
    "The quick brown fox jumps over the lazy dog.",
    "In the beginning was the Word, and the Word was with God.",
    "To be or not to be, that is the question.",
    "All that glitters is not gold.",
    "The only way out is through.",
    "Life is what happens when you're busy making other plans.",
    "In the middle of difficulty lies opportunity.",
    "The best time to plant a tree was twenty years ago; the second best time is now.",
    "You miss one hundred percent of the shots you don't take.",
    "Whether you think you can or you think you can't, you're right.",
]

FAKE_MODELS = [
    "gpt-4o-mini",
    "gpt-4o",
    "meta/llama-3-70b",
    "mistral-7b-instruct",
]


class Message(BaseModel):
    role: str
    content: str


class ChatCompletionRequest(BaseModel):
    model: str = "gpt-4o-mini"
    messages: list[Message]
    max_tokens: int | None = None
    temperature: float | None = None
    stream: bool | None = False


@app.get("/health")
async def health():
    return {"status": "ok"}


@app.get("/v1/models")
async def list_models():
    return {
        "object": "list",
        "data": [
            {
                "id": model_id,
                "object": "model",
                "created": 1700000000,
                "owned_by": "fake-llm",
            }
            for model_id in FAKE_MODELS
        ],
    }


@app.post("/v1/chat/completions")
async def chat_completions(request: ChatCompletionRequest):
    # Simulate realistic inference latency (10–50 ms)
    await asyncio.sleep(random.uniform(0.01, 0.05))

    prompt_tokens = random.randint(50, 500)
    completion_tokens = random.randint(50, 500)
    total_tokens = prompt_tokens + completion_tokens

    response_text = random.choice(FAKE_RESPONSES)
    resolved_model = request.model.split("/")[-1] if "/" in request.model else request.model

    return {
        "id": f"chatcmpl-{uuid.uuid4().hex}",
        "object": "chat.completion",
        "created": int(time.time()),
        "model": resolved_model,
        "choices": [
            {
                "index": 0,
                "message": {
                    "role": "assistant",
                    "content": response_text,
                },
                "logprobs": None,
                "finish_reason": "stop",
            }
        ],
        "usage": {
            "prompt_tokens": prompt_tokens,
            "completion_tokens": completion_tokens,
            "total_tokens": total_tokens,
            "prompt_tokens_details": {
                "cached_tokens": 0,
                "audio_tokens": 0,
            },
            "completion_tokens_details": {
                "reasoning_tokens": 0,
                "audio_tokens": 0,
                "accepted_prediction_tokens": 0,
                "rejected_prediction_tokens": 0,
            },
        },
        "system_fingerprint": f"fp_{uuid.uuid4().hex[:12]}",
    }


@app.exception_handler(Exception)
async def global_exception_handler(request: Request, exc: Exception):
    return JSONResponse(
        status_code=500,
        content={
            "error": {
                "message": str(exc),
                "type": "internal_error",
                "code": "internal_error",
            }
        },
    )
