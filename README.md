# Commit

Commit enhances your WhatsApp usage by finding commitments automatically — things you said you'd do, and things others said they'd do. It auto detects when things are done.

When something goes quiet, it surfaces it for follow-up.

## How it works

You scan a QR code to link your WhatsApp account, same as WhatsApp Web. Commit runs on your machine. Every 10 seconds it reads new messages, analyzes them with a local MLX Gemma 4 model, indexes them with EmbeddingGemma, and logs any commitments it finds.

The dashboard shows everything grouped by person: what you owe, what they owe, how long it's been. You can reply, set reminders, mark things done, or dismiss them.

## Features

- **Dashboard** — all open commitments in one view, grouped by chat. Filter by "I owe", "They owe", or "Resolved". Search across everything.
- **Auto-extraction** — local Gemma 4 reads incoming messages every 10 seconds, identifies promises and obligations, and logs them with the original quote, person, and direction.
- **Semantic search** — EmbeddingGemma indexes WhatsApp text locally so `@find` can use meaning-based retrieval in addition to keyword search.
- **Local Gemma 4** — uses the working 12B MLX path with an MTP draft model for local extraction.
- **Auto-resolution** — when a commitment is fulfilled in conversation (a file shared, a task confirmed, someone says "done"), Commit marks it resolved automatically.
- **Follow-ups** — surfaces things others owe you that have gone quiet. Drafts a polite nudge message and lets you send it directly from the dashboard.
- **Reminders** — set a reminder on any commitment. When it's due, Commit sends you a WhatsApp message to your own account so it shows up in your chat list.
- **Favorites** — star important chats or commitments to pin them to a dedicated tab for quick access.
- **Reply from dashboard** — respond to any commitment's chat directly from the Commit UI without switching to WhatsApp.
- **WhatsApp bot commands** — message yourself on WhatsApp to check commitments, search, or mark things done (see commands below).
- **History sync** — when you link a new device, Commit backfills your recent WhatsApp message history so commitments appear immediately.
- **Dark and light theme** — toggle from the dashboard toolbar. Respects your preference across sessions.
- **Mobile web** — the dashboard is fully responsive. Access it from your phone's browser at the same local address.
- **Passcode protection** — the web interface is secured with a passcode. Local model settings are stored on disk.

## System requirements

- **macOS** 12 Monterey or later on Apple Silicon
- MLX model serving with an OpenAI-compatible `/v1/chat/completions` endpoint
- Commit starts its own local OpenAI-compatible `/v1/embeddings` endpoint backed by EmbeddingGemma
- WhatsApp account with multi-device support

## Install

### Mac (DMG)

Download `Commit-x.x.x.dmg` from Releases, open it, drag Commit to Applications, and open Commit from Applications.

### Windows

Download `Commit-x.x.x-windows-amd64.zip` from Releases, extract it, and run `Commit.exe`.

### From source

Requires Go 1.22+ and local MLX model servers.

```bash
git clone https://github.com/mitensampat/commit.git
cd commit
go build -o commit .
./commit
```

### Local MLX models

Commit defaults to:

- Generation: `mlx-community/gemma-4-12B-it-qat-4bit`
- MTP draft model: `mlx-community/gemma-4-12B-it-qat-assistant-nvfp4`
- Embeddings: `mlx-community/embeddinggemma-300m-4bit`
- Chat endpoint: `http://127.0.0.1:8080/v1`
- Embedding endpoint: `http://127.0.0.1:8081/v1`

Gemma 4 E2B/E4B checkpoints are disabled for now because the current MLX VLM loader rejects their audio-tower weights with a shape mismatch. Commit normalizes those selections back to the working 12B MTP path until upstream support is fixed.

Experimental smaller MLX VLM options:

- `mlx-community/Qwen3-VL-2B-Instruct-4bit`
- `mlx-community/SmolVLM-256M-Instruct-4bit`

These smaller models run without an MTP draft model.

On first run, Commit checks the standard Hugging Face cache and downloads these repos with `hf download` or `huggingface-cli download` if they are missing. Install the Hugging Face Hub CLI with `pipx` first:

```bash
brew install pipx
pipx install "huggingface-hub[hf_xet]"
```

Commit shows dependency/model download status in the setup screen, repairs the local Gemma runtime when needed, then starts `mlx_vlm.server` for chat on port `8080` and a bundled EmbeddingGemma server for semantic search on port `8081`. The bundled `scripts/start-mlx-gemma.sh` remains available for debugging, but normal users should not need a terminal.

Environment overrides:

```bash
COMMIT_LLM_BASE_URL=http://127.0.0.1:8080/v1
COMMIT_EMBEDDING_BASE_URL=http://127.0.0.1:8081/v1
COMMIT_LLM_MODEL=mlx-community/gemma-4-12B-it-qat-4bit
COMMIT_LLM_DRAFT_MODEL=mlx-community/gemma-4-12B-it-qat-assistant-nvfp4
COMMIT_EMBEDDING_MODEL=mlx-community/embeddinggemma-300m-4bit
```

For higher-accuracy local inference with the original MTP setup:

```bash
COMMIT_LLM_MODEL=mlx-community/gemma-4-12B-it-qat-4bit
COMMIT_LLM_DRAFT_MODEL=mlx-community/gemma-4-12B-it-qat-assistant-nvfp4
COMMIT_LLM_NUM_DRAFT_TOKENS=3
```

## Setup

Open [http://commit:9384](http://commit:9384) in your browser (or `localhost:9384`). The setup wizard will walk you through:

1. **Set a passcode** — protects the web interface
2. **Check Local Gemma** — validates the local MLX generation endpoint
3. **Scan the QR code** — links Commit to your WhatsApp account

Once connected, Commit scans incoming messages every 10 seconds and populates the dashboard.

## WhatsApp bot commands

Message yourself on WhatsApp to interact with Commit:

| Command | Description |
|---------|-------------|
| `commitments` | List all open commitments |
| `owe @person` | Show what you owe someone |
| `done <text>` | Mark a commitment as resolved |
| `search <query>` | Find commitments by keyword |
| `help` | Show available commands |

## Architecture

```
main.go              — entry point, wires up components
server/              — HTTP server, API endpoints, auth
server/static/       — embedded web UI (single-page app)
extraction/          — local MLX clients, semantic indexing, commitment extraction prompt
store/               — SQLite database, message and commitment storage
whatsapp/            — WhatsApp client (whatsmeow), bot command handler
landing/             — marketing landing page
```

Data is stored in `~/.commit/`:
- `commit.db` — SQLite database (messages, commitments, settings)
- WhatsApp session files

## Building releases

```bash
# Mac DMG (ad-hoc signed, for local testing)
./scripts/build-mac.sh

# Mac DMG (signed + notarized, for distribution)
DEVELOPER_ID="Developer ID Application: ..." \
NOTARY_PROFILE="commit-notary" \
./scripts/build-mac.sh

# Windows zip
./scripts/build-windows.sh

# Both platforms
./scripts/build-all.sh
```

## Privacy & data

- All data stays on your machine in `~/.commit/` — messages, commitments, settings, and WhatsApp session files
- Messages are decrypted locally by the WhatsApp multi-device protocol
- Message content, sender names, timestamps, embeddings, commitments, and search context stay local when your MLX endpoints are local
- No cloud storage, no telemetry, no tracking by Commit
- Your WhatsApp linked device session persists until you unlink it from your phone (Settings → Linked Devices) — even if Commit is not running, messages will queue for the next session
- To fully remove Commit: unlink the device from WhatsApp, delete the app, and delete `~/.commit/`

## Third-party services

- **MLX / Gemma 4** — local commitment extraction, media descriptions, and nudge generation
- **EmbeddingGemma** — local semantic indexing for WhatsApp search

## License

Private. Not open source.
