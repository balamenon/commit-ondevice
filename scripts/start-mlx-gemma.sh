#!/usr/bin/env bash
set -euo pipefail

export COMMIT_LLM_BASE_URL="${COMMIT_LLM_BASE_URL:-http://127.0.0.1:8080/v1}"
export COMMIT_EMBEDDING_BASE_URL="${COMMIT_EMBEDDING_BASE_URL:-$COMMIT_LLM_BASE_URL}"
export COMMIT_LLM_DRAFT_MODEL="${COMMIT_LLM_DRAFT_MODEL:-mlx-community/gemma-4-12B-it-qat-assistant-nvfp4}"
export COMMIT_LLM_NUM_DRAFT_TOKENS="${COMMIT_LLM_NUM_DRAFT_TOKENS:-3}"

MODEL="${COMMIT_LLM_MODEL:-mlx-community/gemma-4-12B-it-qat-4bit}"
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

find_mlx_server() {
  if command -v mlx_vlm.server >/dev/null 2>&1; then
    command -v mlx_vlm.server
  elif [[ -x "$HOME/.local/bin/mlx_vlm.server" ]]; then
    echo "$HOME/.local/bin/mlx_vlm.server"
  else
    if command -v pipx >/dev/null 2>&1; then
      echo "Installing Gemma 4 MLX runtime..."
      pipx install --force mlx-vlm
      pipx inject --force mlx-vlm 'transformers>=5.5,<5.13' 'huggingface_hub>=1.0'
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
    pipx inject --force mlx-vlm 'transformers>=5.5,<5.13' 'huggingface_hub>=1.0' >/dev/null
  fi
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
    pipx install 'huggingface-hub[hf_xet]'
  else
    echo "Missing Hugging Face CLI. Install pipx or run: pipx install 'huggingface-hub[hf_xet]'" >&2
    return 1
  fi
}

cat <<EOF
Starting MLX server for Commit
  generation model: $MODEL
  MTP draft model:  $COMMIT_LLM_DRAFT_MODEL
  embedding model:  $EMBEDDING_MODEL
  endpoint:         $COMMIT_LLM_BASE_URL

Leave this terminal open while Commit is running.
EOF

echo "Caching models if needed..."
ensure_hf_cli
download_model "$MODEL"
download_model "$COMMIT_LLM_DRAFT_MODEL"
download_model "$EMBEDDING_MODEL"

MLX_SERVER="$(find_mlx_server)"
ensure_mlx_server_healthy "$MLX_SERVER"

exec "$MLX_SERVER" \
  --model "$MODEL" \
  --draft-model "$COMMIT_LLM_DRAFT_MODEL" \
  --draft-kind mtp \
  --host 127.0.0.1 \
  --port 8080
