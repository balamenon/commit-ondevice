#!/usr/bin/env python3
import argparse
import time
from threading import Lock
from typing import Any

import mlx.core as mx
import uvicorn
from fastapi import FastAPI, HTTPException
from mlx_embeddings.utils import load
from pydantic import BaseModel


class EmbeddingRequest(BaseModel):
    model: str
    input: str | list[str]


app = FastAPI()
state: dict[str, Any] = {"model_id": "", "model": None, "tokenizer": None}
state_lock = Lock()


def load_model(model_id: str) -> None:
    with state_lock:
        if state["model"] is not None and state["model_id"] == model_id:
            return
        model, tokenizer = load(model_id)
        state["model_id"] = model_id
        state["model"] = model
        state["tokenizer"] = tokenizer


def encode(model_id: str, texts: list[str]) -> list[list[float]]:
    load_model(model_id)
    with state_lock:
        tokenizer = state["tokenizer"]
        model = state["model"]
        inputs = tokenizer.batch_encode_plus(
            texts,
            return_tensors="mlx",
            padding=True,
            truncation=True,
            max_length=512,
        )
        outputs = model(inputs["input_ids"], attention_mask=inputs["attention_mask"])
        embeddings = outputs.text_embeds
        mx.eval(embeddings)
        return [[float(v) for v in row] for row in embeddings.tolist()]


@app.get("/v1/models")
def models() -> dict[str, Any]:
    model_id = state["model_id"]
    return {
        "object": "list",
        "data": [{"id": model_id, "object": "model", "created": 0, "owned_by": "local"}] if model_id else [],
    }


@app.post("/v1/embeddings")
def embeddings(req: EmbeddingRequest) -> dict[str, Any]:
    if not req.model:
        raise HTTPException(status_code=400, detail="model is required")
    inputs = [req.input] if isinstance(req.input, str) else req.input
    if not inputs:
        raise HTTPException(status_code=400, detail="input is required")
    vectors = encode(req.model, inputs)
    return {
        "object": "list",
        "model": req.model,
        "data": [
            {"object": "embedding", "index": idx, "embedding": vector}
            for idx, vector in enumerate(vectors)
        ],
        "usage": {"prompt_tokens": 0, "total_tokens": 0},
        "created": int(time.time()),
    }


def main() -> None:
    parser = argparse.ArgumentParser(description="Commit local EmbeddingGemma server")
    parser.add_argument("--model", required=True)
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=8081)
    args = parser.parse_args()
    load_model(args.model)
    uvicorn.run(app, host=args.host, port=args.port, log_level="info")


if __name__ == "__main__":
    main()
