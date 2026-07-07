package extraction

import (
	"testing"
	"time"

	"github.com/msfoundry/commit/store"
)

func TestIsSavedSelfBotCommand(t *testing.T) {
	msg := &store.Message{
		ID:        "1",
		ChatJID:   "81973812961508@lid",
		SenderJID: "81973812961508:17@lid",
		Content:   "search chai",
		Timestamp: time.Now(),
		IsFromMe:  true,
	}
	if !isSavedSelfBotCommand(msg) {
		t.Fatal("expected saved self-chat bot command to be skipped")
	}
}

func TestIsSavedSelfBotCommandKeepsNormalOutbound(t *testing.T) {
	msg := &store.Message{
		ID:        "1",
		ChatJID:   "36034809213160@lid",
		SenderJID: "81973812961508:17@lid",
		Content:   "search chai",
		Timestamp: time.Now(),
		IsFromMe:  true,
	}
	if isSavedSelfBotCommand(msg) {
		t.Fatal("expected outbound command-like text in another chat to be kept")
	}
}
