package store

import (
	"time"
)

type MediaAsset struct {
	ID          string
	MessageID   string
	ChatJID     string
	MediaType   string
	MimeType    string
	FileName    string
	Path        string
	Caption     string
	Description string
	Timestamp   time.Time
}

func (db *DB) SaveMediaAsset(asset *MediaAsset) error {
	if asset == nil || asset.ID == "" || asset.MessageID == "" || asset.Path == "" {
		return nil
	}
	_, err := db.conn.Exec(`
		INSERT OR IGNORE INTO media_assets
			(id, message_id, chat_jid, media_type, mime_type, file_name, path, caption, timestamp, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		asset.ID, asset.MessageID, asset.ChatJID, asset.MediaType, asset.MimeType, asset.FileName,
		asset.Path, asset.Caption, asset.Timestamp.Unix(), time.Now().Unix(),
	)
	return err
}

func (db *DB) GetUndescribedMediaAssets(limit int) ([]*MediaAsset, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := db.conn.Query(`
		SELECT id, message_id, chat_jid, media_type, mime_type, file_name, path, caption, description, timestamp
		FROM media_assets
		WHERE described_at IS NULL
		ORDER BY timestamp ASC
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var assets []*MediaAsset
	for rows.Next() {
		asset := &MediaAsset{}
		var ts int64
		if err := rows.Scan(&asset.ID, &asset.MessageID, &asset.ChatJID, &asset.MediaType, &asset.MimeType,
			&asset.FileName, &asset.Path, &asset.Caption, &asset.Description, &ts); err != nil {
			return nil, err
		}
		asset.Timestamp = time.Unix(ts, 0)
		assets = append(assets, asset)
	}
	return assets, rows.Err()
}

func (db *DB) SetMediaDescription(id, description string) error {
	_, err := db.conn.Exec(
		"UPDATE media_assets SET description = ?, described_at = ? WHERE id = ?",
		description, time.Now().Unix(), id,
	)
	return err
}
