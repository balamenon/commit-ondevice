package whatsapp

import (
	"testing"

	"go.mau.fi/whatsmeow"
	waStore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func TestIsSelfChatMatchesOwnPhoneOrLID(t *testing.T) {
	phone := types.NewJID("12345", types.DefaultUserServer)
	lid := types.NewJID("99999", types.HiddenUserServer)
	client := &Client{
		wa: &whatsmeow.Client{
			Store: &waStore.Device{
				ID:  &phone,
				LID: lid,
			},
		},
	}

	tests := []struct {
		name string
		chat types.JID
	}{
		{name: "phone chat", chat: types.NewJID("12345", types.DefaultUserServer)},
		{name: "lid chat", chat: types.NewJID("99999", types.HiddenUserServer)},
		{name: "hosted phone chat", chat: types.NewJID("12345", types.HostedServer)},
		{name: "hosted lid chat", chat: types.NewJID("99999", types.HostedLIDServer)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			evt := &events.Message{
				Info: types.MessageInfo{
					MessageSource: types.MessageSource{Chat: tt.chat},
				},
			}
			if !client.isSelfChat(evt) {
				t.Fatalf("expected %s to be treated as self-chat", tt.chat)
			}
		})
	}
}

func TestIsSelfChatDoesNotMatchOneToOneContact(t *testing.T) {
	phone := types.NewJID("12345", types.DefaultUserServer)
	lid := types.NewJID("99999", types.HiddenUserServer)
	client := &Client{
		wa: &whatsmeow.Client{
			Store: &waStore.Device{
				ID:  &phone,
				LID: lid,
			},
		},
	}

	tests := []struct {
		name string
		chat types.JID
	}{
		{name: "phone contact", chat: types.NewJID("54321", types.DefaultUserServer)},
		{name: "lid contact", chat: types.NewJID("88888", types.HiddenUserServer)},
		{name: "group", chat: types.NewJID("12345-67890", types.GroupServer)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			evt := &events.Message{
				Info: types.MessageInfo{
					MessageSource: types.MessageSource{
						Chat:   tt.chat,
						Sender: tt.chat,
					},
				},
			}
			if client.isSelfChat(evt) {
				t.Fatalf("expected %s to stay a normal chat", tt.chat)
			}
		})
	}
}
