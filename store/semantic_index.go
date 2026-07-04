package store

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"
)

type SemanticDocument struct {
	SourceType string
	SourceID   string
	ChatJID    string
	Title      string
	Content    string
	Timestamp  time.Time
}

type SemanticMatch struct {
	SourceType string
	SourceID   string
	ChatJID    string
	Title      string
	Content    string
	Timestamp  time.Time
	Score      float64
}

func (db *DB) SaveSemanticDocument(doc *SemanticDocument, model string, embedding []float64) error {
	if doc == nil || doc.SourceType == "" || doc.SourceID == "" || doc.Content == "" || len(embedding) == 0 {
		return nil
	}
	data, err := json.Marshal(embedding)
	if err != nil {
		return err
	}
	id := fmt.Sprintf("%s:%s:%s", doc.SourceType, doc.SourceID, model)
	_, err = db.conn.Exec(`
		INSERT INTO semantic_index
			(id, source_type, source_id, chat_jid, title, content, timestamp, embedding_model, dimensions, embedding, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(source_type, source_id, embedding_model) DO UPDATE SET
			chat_jid = excluded.chat_jid,
			title = excluded.title,
			content = excluded.content,
			timestamp = excluded.timestamp,
			dimensions = excluded.dimensions,
			embedding = excluded.embedding,
			updated_at = excluded.updated_at`,
		id, doc.SourceType, doc.SourceID, doc.ChatJID, doc.Title, doc.Content, doc.Timestamp.Unix(),
		model, len(embedding), string(data), time.Now().Unix(),
	)
	return err
}

func (db *DB) GetUnindexedMessages(model string, limit int) ([]*Message, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := db.conn.Query(`
		SELECT m.id, m.chat_jid, m.sender_jid, m.sender_name, m.chat_name, m.content, m.timestamp, m.is_from_me, m.is_group
		FROM messages m
		LEFT JOIN semantic_index si
			ON si.source_type = 'message'
			AND si.source_id = m.id
			AND si.embedding_model = ?
		WHERE si.id IS NULL AND m.content != ''
		ORDER BY m.timestamp ASC
		LIMIT ?`, model, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []*Message
	for rows.Next() {
		m := &Message{}
		var ts int64
		var fromMe, group int
		if err := rows.Scan(&m.ID, &m.ChatJID, &m.SenderJID, &m.SenderName, &m.ChatName,
			&m.Content, &ts, &fromMe, &group); err != nil {
			return nil, err
		}
		m.Timestamp = time.Unix(ts, 0)
		m.IsFromMe = fromMe == 1
		m.IsGroup = group == 1
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

func (db *DB) SearchSemanticIndex(query []float64, model string, limit int) ([]*SemanticMatch, error) {
	if len(query) == 0 || limit <= 0 {
		return nil, nil
	}
	rows, err := db.conn.Query(`
		SELECT source_type, source_id, chat_jid, title, content, timestamp, embedding
		FROM semantic_index
		WHERE embedding_model = ?`, model)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var matches []*SemanticMatch
	for rows.Next() {
		var raw string
		m := &SemanticMatch{}
		var ts int64
		if err := rows.Scan(&m.SourceType, &m.SourceID, &m.ChatJID, &m.Title, &m.Content, &ts, &raw); err != nil {
			return nil, err
		}
		var emb []float64
		if err := json.Unmarshal([]byte(raw), &emb); err != nil {
			continue
		}
		m.Timestamp = time.Unix(ts, 0)
		m.Score = cosine(query, emb)
		matches = append(matches, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	sort.Slice(matches, func(i, j int) bool {
		if matches[i].Score == matches[j].Score {
			return matches[i].Timestamp.After(matches[j].Timestamp)
		}
		return matches[i].Score > matches[j].Score
	})
	if len(matches) > limit {
		matches = matches[:limit]
	}
	return matches, nil
}

func cosine(a, b []float64) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, aa, bb float64
	for i := range a {
		dot += a[i] * b[i]
		aa += a[i] * a[i]
		bb += b[i] * b[i]
	}
	if aa == 0 || bb == 0 {
		return 0
	}
	return dot / (math.Sqrt(aa) * math.Sqrt(bb))
}
