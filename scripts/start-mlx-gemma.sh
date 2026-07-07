#!/usr/bin/env bash
set -euo pipefail

export COMMIT_LLM_BASE_URL="${COMMIT_LLM_BASE_URL:-http://127.0.0.1:8080/v1}"
export COMMIT_EMBEDDING_BASE_URL="${COMMIT_EMBEDDING_BASE_URL:-http://127.0.0.1:8081/v1}"
export COMMIT_LLM_DRAFT_MODEL="${COMMIT_LLM_DRAFT_MODEL:-none}"
export COMMIT_LLM_NUM_DRAFT_TOKENS="${COMMIT_LLM_NUM_DRAFT_TOKENS:-3}"

MODEL="${COMMIT_LLM_MODEL:-mlx-community/gemma-4-e2b-it-4bit}"
EMBEDDING_MODEL="${COMMIT_EMBEDDING_MODEL:-mlx-community/embeddinggemma-300m-4bit}"

hf_cache_dir() {
  if [[ -n "${HUGGINGFACE_HUB_CACHE:-}" ]]; then
    echo "$HUGGINGFACE_HUB_CACHE"
  elif [[ -n "${HF_HOME:-}" ]]; then
    echo "$HF_HOME/hub"
  else
    echo "$HOME/.cache/huggingface/hub"
  fi
}

model_cache_path() {
  local model="$1"
  echo "$(hf_cache_dir)/models--${model//\//--}"
}

is_model_cached() {
  local model_path
  model_path="$(model_cache_path "$1")"
  compgen -G "$model_path/snapshots/*" >/dev/null
}

download_in_progress() {
  local model="$1"
  pgrep -f "hf download ${model}" >/dev/null 2>&1 || \
    pgrep -f "huggingface-cli download ${model}" >/dev/null 2>&1
}

wait_for_model_cache() {
  local model="$1"
  while download_in_progress "$model"; do
    if is_model_cached "$model"; then
      return
    fi
    echo "Another process is downloading $model; waiting..."
    sleep 10
  done
}

download_model() {
  local model="$1"
  if [[ -z "$model" || "$model" == "none" ]]; then
    return
  fi
  if is_model_cached "$model"; then
    echo "Model already cached: $model"
    return
  fi
  wait_for_model_cache "$model"
  if is_model_cached "$model"; then
    echo "Model cached by another process: $model"
    return
  fi
  if command -v hf >/dev/null 2>&1; then
    "$(command -v hf)" download "$model" >/dev/null
  elif [[ -x "$HOME/.local/bin/hf" ]]; then
    "$HOME/.local/bin/hf" download "$model" >/dev/null
  elif command -v huggingface-cli >/dev/null 2>&1; then
    "$(command -v huggingface-cli)" download "$model" >/dev/null
  elif [[ -x "$HOME/.local/bin/huggingface-cli" ]]; then
    "$HOME/.local/bin/huggingface-cli" download "$model" >/dev/null
  else
    echo "Missing Hugging Face CLI. Install with: pipx install 'huggingface-hub[hf_xet]'" >&2
    return 1
  fi
}

pipx_python_args() {
  for py in python3.13 python3.12 python3.11 /opt/homebrew/bin/python3.13 /opt/homebrew/bin/python3.12 /opt/homebrew/bin/python3.11 /usr/local/bin/python3.13 /usr/local/bin/python3.12 /usr/local/bin/python3.11; do
    if command -v "$py" >/dev/null 2>&1 || [[ -x "$py" ]]; then
      echo --python "$py" --fetch-python missing
      return
    fi
  done
  echo --python 3.13 --fetch-python missing
}

find_mlx_server() {
  if command -v mlx_vlm.server >/dev/null 2>&1; then
    command -v mlx_vlm.server
  elif [[ -x "$HOME/.local/bin/mlx_vlm.server" ]]; then
    echo "$HOME/.local/bin/mlx_vlm.server"
  else
    if command -v pipx >/dev/null 2>&1; then
      echo "Installing Gemma 4 MLX runtime..."
      pipx install $(pipx_python_args) --force mlx-vlm
      pipx inject --force mlx-vlm 'transformers==5.5.0' 'huggingface_hub>=1.0' torch torchvision
    else
      echo "Missing MLX server. Install pipx or run: pipx install mlx-vlm" >&2
      return 1
    fi
    if [[ -x "$HOME/.local/bin/mlx_vlm.server" ]]; then
      echo "$HOME/.local/bin/mlx_vlm.server"
    else
      command -v mlx_vlm.server
    fi
  fi
}

repair_mlx_vlm() {
  if command -v pipx >/dev/null 2>&1; then
    pipx inject --force mlx-vlm 'transformers==5.5.0' 'huggingface_hub>=1.0' >/dev/null
    pipx inject --force mlx-vlm torch torchvision >/dev/null
  fi
}

ensure_embedding_runtime() {
  local python="$1"
  if "$python" -c 'import fastapi, uvicorn, mlx_embeddings' >/dev/null 2>&1; then
    return
  fi
  if command -v pipx >/dev/null 2>&1; then
    echo "Installing EmbeddingGemma MLX runtime..."
    pipx inject --force mlx-vlm mlx-embeddings fastapi uvicorn
  else
    echo "Missing embedding runtime. Install pipx or run: pipx inject mlx-vlm mlx-embeddings fastapi uvicorn" >&2
    return 1
  fi
  "$python" -c 'import fastapi, uvicorn, mlx_embeddings' >/dev/null
}

mlx_vlm_python() {
  if [[ -x "$HOME/.local/pipx/venvs/mlx-vlm/bin/python" ]]; then
    echo "$HOME/.local/pipx/venvs/mlx-vlm/bin/python"
    return
  fi
  command -v python3
}

ensure_mlx_server_healthy() {
  local server="$1"
  if "$server" --help >/dev/null 2>&1; then
    return
  fi
  echo "Repairing Gemma 4 MLX runtime..."
  repair_mlx_vlm
  "$server" --help >/dev/null
}

ensure_hf_cli() {
  if command -v hf >/dev/null 2>&1 || [[ -x "$HOME/.local/bin/hf" ]]; then
    return
  fi
  if command -v pipx >/dev/null 2>&1; then
    echo "Installing Hugging Face downloader..."
    pipx install $(pipx_python_args) 'huggingface-hub[hf_xet]'
  else
    echo "Missing Hugging Face CLI. Install pipx or run: pipx install 'huggingface-hub[hf_xet]'" >&2
    return 1
  fi
}

cat <<EOF
Starting MLX server for Commit
  generation model: $MODEL
  MTP draft model:  ${COMMIT_LLM_DRAFT_MODEL}
  embedding model:  $EMBEDDING_MODEL
  chat endpoint:    $COMMIT_LLM_BASE_URL
  embedding endpoint: $COMMIT_EMBEDDING_BASE_URL

Leave this terminal open while Commit is running.
EOF

echo "Caching models if needed..."
ensure_hf_cli
download_model "$MODEL"
download_model "$COMMIT_LLM_DRAFT_MODEL"
download_model "$EMBEDDING_MODEL"

MLX_SERVER="$(find_mlx_server)"
ensure_mlx_server_healthy "$MLX_SERVER"
EMBEDDING_PYTHON="$(mlx_vlm_python)"
ensure_embedding_runtime "$EMBEDDING_PYTHON"

ARGS=(--model "$MODEL" --host 127.0.0.1 --port 8080)
if [[ -n "$COMMIT_LLM_DRAFT_MODEL" && "$COMMIT_LLM_DRAFT_MODEL" != "none" ]]; then
  ARGS+=(--draft-model "$COMMIT_LLM_DRAFT_MODEL" --draft-kind mtp)
fi

"$EMBEDDING_PYTHON" "$(dirname "$0")/embedding_server.py" \
  --model "$EMBEDDING_MODEL" \
  --host 127.0.0.1 \
  --port 8081 &
EMBEDDING_PID="$!"
trap 'kill "$EMBEDDING_PID" 2>/dev/null || true' EXIT

exec "$MLX_SERVER" "${ARGS[@]}"
