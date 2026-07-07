package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/crypto/pbkdf2"

	_ "modernc.org/sqlite"
)

type DB struct {
	conn         *sql.DB
	cryptoMu     sync.RWMutex
	cryptoKey    []byte // derived from passcode, set after auth
	keyCachePath string // 0600 file caching the derived key across restarts
}

func Open(path string) (*DB, error) {
	conn, err := sql.Open("sqlite", path+"?_pragma=journal_mode(wal)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	db := &DB{conn: conn, keyCachePath: filepath.Join(filepath.Dir(path), ".crypto_key")}
	if err := db.migrate(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	db.ensureMachineKey()
	db.loadCachedKey()
	return db, nil
}

// loadCachedKey restores the passcode-derived crypto key from disk so a
// restart doesn't leave the API key undecryptable until the next web login.
// Web sessions persist across restarts, so that login may never come.
func (db *DB) loadCachedKey() {
	if db.IsUnlocked() || db.keyCachePath == "" {
		return
	}
	data, err := os.ReadFile(db.keyCachePath)
	if err != nil {
		return
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(data)))
	if err != nil || len(key) != 32 {
		return
	}
	db.cryptoMu.Lock()
	db.cryptoKey = key
	db.cryptoMu.Unlock()
}

func (db *DB) cacheKey(key []byte) {
	if db.keyCachePath == "" {
		return
	}
	os.WriteFile(db.keyCachePath, []byte(hex.EncodeToString(key)), 0600)
}

func (db *DB) Close() error {
	return db.conn.Close()
}

func (db *DB) migrate() error {
	_, err := db.conn.Exec(`
		CREATE TABLE IF NOT EXISTS settings (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		);

		CREATE TABLE IF NOT EXISTS messages (
			id         TEXT PRIMARY KEY,
			chat_jid   TEXT NOT NULL,
			sender_jid TEXT NOT NULL,
			sender_name TEXT NOT NULL DEFAULT '',
			chat_name  TEXT NOT NULL DEFAULT '',
			content    TEXT NOT NULL,
			timestamp  INTEGER NOT NULL,
			is_from_me INTEGER NOT NULL DEFAULT 0,
			is_group   INTEGER NOT NULL DEFAULT 0,
			processed  INTEGER NOT NULL DEFAULT 0
		);

		CREATE INDEX IF NOT EXISTS idx_messages_chat ON messages(chat_jid);
		CREATE INDEX IF NOT EXISTS idx_messages_ts ON messages(timestamp);
		CREATE INDEX IF NOT EXISTS idx_messages_unprocessed ON messages(processed) WHERE processed = 0;

		CREATE TABLE IF NOT EXISTS commitments (
			id          TEXT PRIMARY KEY,
			chat_jid    TEXT NOT NULL,
			chat_name   TEXT NOT NULL DEFAULT '',
			person_name TEXT NOT NULL,
			person_jid  TEXT NOT NULL DEFAULT '',
			title       TEXT NOT NULL,
			context     TEXT NOT NULL DEFAULT '',
			direction   TEXT NOT NULL CHECK(direction IN ('you_owe', 'they_owe')),
			source_quote TEXT NOT NULL DEFAULT '',
			source_time TEXT NOT NULL DEFAULT '',
			message_id  TEXT NOT NULL DEFAULT '',
			status      TEXT NOT NULL DEFAULT 'open' CHECK(status IN ('open', 'resolved', 'dismissed', 'snoozed')),
			due_hint    TEXT NOT NULL DEFAULT '',
			created_at  INTEGER NOT NULL,
				resolved_at INTEGER,
				is_group    INTEGER NOT NULL DEFAULT 0,
				favorited   INTEGER NOT NULL DEFAULT 0,
				snoozed_flag INTEGER NOT NULL DEFAULT 0
			);

		CREATE INDEX IF NOT EXISTS idx_commitments_status ON commitments(status);
		CREATE INDEX IF NOT EXISTS idx_commitments_person ON commitments(person_name);
		CREATE INDEX IF NOT EXISTS idx_commitments_chat ON commitments(chat_jid);
		CREATE TABLE IF NOT EXISTS favorite_chats (
			chat_jid  TEXT PRIMARY KEY,
			chat_name TEXT NOT NULL DEFAULT '',
			is_group  INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL DEFAULT 0
		);

		CREATE TABLE IF NOT EXISTS semantic_index (
			id              TEXT PRIMARY KEY,
			source_type     TEXT NOT NULL,
			source_id       TEXT NOT NULL,
			chat_jid        TEXT NOT NULL DEFAULT '',
			title           TEXT NOT NULL DEFAULT '',
			content         TEXT NOT NULL,
			timestamp       INTEGER NOT NULL DEFAULT 0,
			embedding_model TEXT NOT NULL,
			dimensions      INTEGER NOT NULL,
			embedding       TEXT NOT NULL,
			updated_at      INTEGER NOT NULL,
			UNIQUE(source_type, source_id, embedding_model)
		);

		CREATE INDEX IF NOT EXISTS idx_semantic_source ON semantic_index(source_type, source_id);
		CREATE INDEX IF NOT EXISTS idx_semantic_chat ON semantic_index(chat_jid, timestamp);

		CREATE TABLE IF NOT EXISTS media_assets (
			id          TEXT PRIMARY KEY,
			message_id  TEXT NOT NULL,
			chat_jid    TEXT NOT NULL DEFAULT '',
			media_type  TEXT NOT NULL DEFAULT '',
			mime_type   TEXT NOT NULL DEFAULT '',
			file_name   TEXT NOT NULL DEFAULT '',
			path        TEXT NOT NULL,
			caption     TEXT NOT NULL DEFAULT '',
			description TEXT NOT NULL DEFAULT '',
			timestamp   INTEGER NOT NULL DEFAULT 0,
			created_at  INTEGER NOT NULL,
			described_at INTEGER,
			UNIQUE(message_id, path)
		);

			CREATE INDEX IF NOT EXISTS idx_media_message ON media_assets(message_id);
			CREATE INDEX IF NOT EXISTS idx_media_pending ON media_assets(described_at) WHERE described_at IS NULL;

			CREATE TABLE IF NOT EXISTS sessions (
				token_hash TEXT PRIMARY KEY,
				expires_at INTEGER NOT NULL
			);
		`)
	if err != nil {
		return err
	}

	version := db.schemaVersion()

	if version < 1 {
		db.conn.Exec("ALTER TABLE commitments ADD COLUMN favorited INTEGER NOT NULL DEFAULT 0")
		db.conn.Exec("ALTER TABLE commitments ADD COLUMN resolved_by TEXT NOT NULL DEFAULT 'user'")
		db.conn.Exec("ALTER TABLE commitments ADD COLUMN last_nudged_at INTEGER")
		db.conn.Exec("ALTER TABLE commitments ADD COLUMN reminder_at INTEGER")
	}

	if version < 2 {
		db.conn.Exec(`CREATE TABLE IF NOT EXISTS muted_chats (
			chat_jid   TEXT PRIMARY KEY,
			chat_name  TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL DEFAULT 0
		)`)
	}

	if version < 3 {
		db.conn.Exec("ALTER TABLE commitments ADD COLUMN resolved_by TEXT NOT NULL DEFAULT 'user'")
		db.conn.Exec("ALTER TABLE commitments ADD COLUMN last_nudged_at INTEGER")
		db.conn.Exec("ALTER TABLE commitments ADD COLUMN reminder_at INTEGER")
	}

	if version < 4 {
		db.conn.Exec("CREATE INDEX IF NOT EXISTS idx_messages_chat_inbound ON messages(chat_jid, timestamp) WHERE is_from_me = 0")
		db.conn.Exec("CREATE INDEX IF NOT EXISTS idx_commitments_open_direction ON commitments(status, direction, created_at) WHERE status = 'open'")
	}

	if version < 5 {
		db.conn.Exec("ALTER TABLE commitments ADD COLUMN significance TEXT NOT NULL DEFAULT 'medium'")
	}

	if version < 6 {
		// Distinguishes a silent snooze (resurfaces quietly when due) from a
		// user-set reminder (pings the self-chat when due).
		db.conn.Exec("ALTER TABLE commitments ADD COLUMN snoozed_flag INTEGER NOT NULL DEFAULT 0")
	}

	if version < 7 {
		// Web sessions persist across restarts. Only the SHA-256 of the
		// token is stored, so reading the DB can't impersonate a session.
		db.conn.Exec(`CREATE TABLE IF NOT EXISTS sessions (
			token_hash TEXT PRIMARY KEY,
			expires_at INTEGER NOT NULL
		)`)
	}

	if version < 8 {
		// Compatibility for databases created by the on-device fork before
		// upstream added snoozes and persistent sessions.
		db.conn.Exec("ALTER TABLE commitments ADD COLUMN snoozed_flag INTEGER NOT NULL DEFAULT 0")
		db.conn.Exec(`CREATE TABLE IF NOT EXISTS sessions (
			token_hash TEXT PRIMARY KEY,
			expires_at INTEGER NOT NULL
		)`)
		db.conn.Exec(`CREATE TABLE IF NOT EXISTS semantic_index (
			id              TEXT PRIMARY KEY,
			source_type     TEXT NOT NULL,
			source_id       TEXT NOT NULL,
			chat_jid        TEXT NOT NULL DEFAULT '',
			title           TEXT NOT NULL DEFAULT '',
			content         TEXT NOT NULL,
			timestamp       INTEGER NOT NULL DEFAULT 0,
			embedding_model TEXT NOT NULL,
			dimensions      INTEGER NOT NULL,
			embedding       TEXT NOT NULL,
			updated_at      INTEGER NOT NULL,
			UNIQUE(source_type, source_id, embedding_model)
		)`)
		db.conn.Exec("CREATE INDEX IF NOT EXISTS idx_semantic_source ON semantic_index(source_type, source_id)")
		db.conn.Exec("CREATE INDEX IF NOT EXISTS idx_semantic_chat ON semantic_index(chat_jid, timestamp)")
	}

	if version < 9 {
		db.conn.Exec(`CREATE TABLE IF NOT EXISTS media_assets (
			id          TEXT PRIMARY KEY,
			message_id  TEXT NOT NULL,
			chat_jid    TEXT NOT NULL DEFAULT '',
			media_type  TEXT NOT NULL DEFAULT '',
			mime_type   TEXT NOT NULL DEFAULT '',
			file_name   TEXT NOT NULL DEFAULT '',
			path        TEXT NOT NULL,
			caption     TEXT NOT NULL DEFAULT '',
			description TEXT NOT NULL DEFAULT '',
			timestamp   INTEGER NOT NULL DEFAULT 0,
			created_at  INTEGER NOT NULL,
			described_at INTEGER,
			UNIQUE(message_id, path)
		)`)
		db.conn.Exec("CREATE INDEX IF NOT EXISTS idx_media_message ON media_assets(message_id)")
		db.conn.Exec("CREATE INDEX IF NOT EXISTS idx_media_pending ON media_assets(described_at) WHERE described_at IS NULL")
	}

	db.setSchemaVersion(9)
	return nil
}

func (db *DB) schemaVersion() int {
	var v int
	db.conn.QueryRow("SELECT CAST(value AS INTEGER) FROM settings WHERE key = 'schema_version'").Scan(&v)
	return v
}

func (db *DB) setSchemaVersion(v int) {
	db.SetSetting("schema_version", fmt.Sprintf("%d", v))
}

// Settings

func (db *DB) GetSetting(key string) string {
	var val string
	db.conn.QueryRow("SELECT value FROM settings WHERE key = ?", key).Scan(&val)
	return val
}

func (db *DB) SetSetting(key, value string) error {
	_, err := db.conn.Exec(
		"INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
		key, value,
	)
	return err
}

// Passcode

func (db *DB) HasPasscode() bool {
	return db.GetSetting("passcode_hash") != ""
}

func (db *DB) SetPasscode(passcode string) error {
	// Decrypt existing data with current key before switching
	existingAPIKey := db.GetAPIKey()

	hash, err := bcrypt.GenerateFromPassword([]byte(passcode), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	if err := db.SetSetting("passcode_hash", string(hash)); err != nil {
		return err
	}
	salt := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return err
	}
	if err := db.SetSetting("crypto_salt", hex.EncodeToString(salt)); err != nil {
		return err
	}
	db.deriveKey(passcode)
	db.conn.Exec("DELETE FROM settings WHERE key = 'machine_key'")

	// Re-encrypt existing data with the new passcode-derived key
	if existingAPIKey != "" {
		if err := db.SetAPIKey(existingAPIKey); err != nil {
			return fmt.Errorf("re-encrypt local credential: %w", err)
		}
	}
	return nil
}

func (db *DB) CheckPasscode(passcode string) bool {
	hash := db.GetSetting("passcode_hash")
	if hash == "" {
		return false
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(passcode)); err != nil {
		return false
	}
	db.deriveKey(passcode)
	return true
}

func (db *DB) deriveKey(passcode string) {
	saltHex := db.GetSetting("crypto_salt")
	salt, _ := hex.DecodeString(saltHex)
	if len(salt) == 0 {
		return
	}
	key := pbkdf2.Key([]byte(passcode), salt, 100000, 32, sha256.New)
	db.cryptoMu.Lock()
	db.cryptoKey = key
	db.cryptoMu.Unlock()
	db.cacheKey(key)
}

func (db *DB) ensureMachineKey() {
	if db.HasPasscode() {
		return
	}
	existing := db.GetSetting("machine_key")
	if existing != "" {
		key, _ := hex.DecodeString(existing)
		if len(key) == 32 {
			db.cryptoMu.Lock()
			db.cryptoKey = key
			db.cryptoMu.Unlock()
			return
		}
	}
	key := make([]byte, 32)
	io.ReadFull(rand.Reader, key)
	db.SetSetting("machine_key", hex.EncodeToString(key))
	db.cryptoMu.Lock()
	db.cryptoKey = key
	db.cryptoMu.Unlock()
}

func (db *DB) IsUnlocked() bool {
	db.cryptoMu.RLock()
	defer db.cryptoMu.RUnlock()
	return len(db.cryptoKey) > 0
}

func (db *DB) encrypt(plaintext string) (string, error) {
	db.cryptoMu.RLock()
	key := db.cryptoKey
	db.cryptoMu.RUnlock()
	if len(key) == 0 {
		return "", fmt.Errorf("no encryption key available")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aesGCM.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ciphertext := aesGCM.Seal(nonce, nonce, []byte(plaintext), nil)
	return "enc:" + hex.EncodeToString(ciphertext), nil
}

func (db *DB) decrypt(stored string) (string, error) {
	db.cryptoMu.RLock()
	key := db.cryptoKey
	db.cryptoMu.RUnlock()
	if len(key) == 0 || len(stored) < 4 || stored[:4] != "enc:" {
		return stored, nil
	}
	data, err := hex.DecodeString(stored[4:])
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonceSize := aesGCM.NonceSize()
	if len(data) < nonceSize {
		return "", fmt.Errorf("ciphertext too short")
	}
	plaintext, err := aesGCM.Open(nil, data[:nonceSize], data[nonceSize:], nil)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

// Model

const Gemma4E2BModel = "mlx-community/gemma-4-e2b-it-4bit"
const Gemma4E4BModel = "mlx-community/gemma-4-e4b-it-4bit"
const Gemma412BModel = "mlx-community/gemma-4-12B-it-qat-4bit"
const Gemma412BDraftModel = "mlx-community/gemma-4-12B-it-qat-assistant-nvfp4"

const DefaultModel = Gemma4E2BModel
const FallbackModel = DefaultModel
const DefaultDraftModel = "none"
const DefaultEmbeddingModel = "mlx-community/embeddinggemma-300m-4bit"

func DefaultDraftForModel(model string) string {
	if model == Gemma412BModel {
		return Gemma412BDraftModel
	}
	return DefaultDraftModel
}

func (db *DB) GetModel() string {
	m := db.GetSetting("llm_model")
	if m == "" {
		m = db.GetSetting("claude_model")
	}
	if m == "" {
		return DefaultModel
	}
	return m
}

func (db *DB) SetModel(model string) error {
	return db.SetSetting("llm_model", model)
}

func (db *DB) GetEmbeddingModel() string {
	m := db.GetSetting("embedding_model")
	if m == "" {
		return DefaultEmbeddingModel
	}
	return m
}

func (db *DB) SetEmbeddingModel(model string) error {
	return db.SetSetting("embedding_model", model)
}

// My Style

func (db *DB) GetMyStyle() string {
	return db.GetSetting("my_style")
}

func (db *DB) SetMyStyle(style string) error {
	return db.SetSetting("my_style", style)
}

// Contact Overrides — map chat_jid to display name

func (db *DB) GetContactOverrides() map[string]string {
	raw := db.GetSetting("contact_overrides")
	if raw == "" {
		return map[string]string{}
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return map[string]string{}
	}
	return m
}

func (db *DB) SetContactOverrides(overrides map[string]string) error {
	data, err := json.Marshal(overrides)
	if err != nil {
		return err
	}
	return db.SetSetting("contact_overrides", string(data))
}

// API Key (encrypted at rest)

func (db *DB) GetAPIKey() string {
	stored := db.GetSetting("api_key")
	if stored == "" {
		return ""
	}
	decrypted, err := db.decrypt(stored)
	if err != nil {
		return stored
	}
	return decrypted
}

func (db *DB) SetAPIKey(key string) error {
	encrypted, err := db.encrypt(key)
	if err != nil {
		return err
	}
	return db.SetSetting("api_key", encrypted)
}
