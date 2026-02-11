package connector

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/event"

	"go.mau.fi/mautrix-meta/pkg/messagix/table"
	"go.mau.fi/mautrix-meta/pkg/metaid"
)

var cmdToggleEncryption = &commands.FullHandler{
	Func: fnToggleEncryption,
	Name: "toggle-encryption",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionChats,
		Description: "Toggle Messenger-side encryption for the current room",
	},
	RequiresPortal: true,
	RequiresLogin:  true,
}

func fnToggleEncryption(ce *commands.Event) {
	conn := ce.Bridge.Network.(*MetaConnector)
	if !conn.Config.Mode.IsMessenger() {
		ce.Reply("Instagram does not support encryption")
		return
	} else if ce.Portal.RoomType != database.RoomTypeDM {
		ce.Reply("Only private chats can be toggled between encrypted and unencrypted")
		return
	}
	login, _, err := ce.Portal.FindPreferredLogin(ce.Ctx, ce.User, false)
	if err != nil {
		ce.Reply("Failed to find login for room")
		ce.Log.Err(err).Msg("Failed to find login for room")
		return
	}
	cli := login.Client.(*MetaClient)
	meta := ce.Portal.Metadata.(*metaid.PortalMetadata)
	if meta.ThreadType.IsWhatsApp() {
		meta.ThreadType = table.ONE_TO_ONE
		ce.Reply("Messages in this room will now be sent unencrypted over Messenger")
	} else {
		if len(ce.Args) == 0 || ce.Args[0] != "--force" {
			threadID := metaid.ParseFBPortalID(ce.Portal.ID)
			err = cli.CreateWhatsAppDM(ce.Ctx, threadID)
			if err != nil {
				ce.Log.Err(err).Msg("Failed to create WhatsApp thread")
				ce.Reply("Failed to create WhatsApp thread")
			}
		}
		meta.ThreadType = table.ENCRYPTED_OVER_WA_ONE_TO_ONE
		ce.Reply("Messages in this room will now be sent encrypted over WhatsApp")
	}
	err = ce.Portal.Save(ce.Ctx)
	if err != nil {
		ce.Log.Err(err).Msg("Failed to update portal in database")
	}
}

var cmdImportMessages = &commands.FullHandler{
	Func: fnImportMessages,
	Name: "import-messages",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionChats,
		Description: "Import messages from a Messenger JSON export into the current room",
	},
	RequiresPortal: true,
	RequiresLogin:  true,
}

type messengerExport struct {
	Participants []struct {
		Name string `json:"name"`
	} `json:"participants"`
	Messages []messengerMessage `json:"messages"`
}

type messengerMessage struct {
	SenderName  string `json:"senderName"`
	Text        string `json:"text"`
	Timestamp   int64  `json:"timestamp"`
	Type        string `json:"type"`
	IsUnsent    bool   `json:"isUnsent"`
}

func fnImportMessages(ce *commands.Event) {
	if ce.Portal.RoomType != database.RoomTypeDM {
		ce.Reply("This command only works in DM rooms")
		return
	}
	if ce.RawArgs == "" {
		ce.Reply("Usage: import-messages <JSON>")
		return
	}

	var export messengerExport
	if err := json.Unmarshal([]byte(ce.RawArgs), &export); err != nil {
		ce.Reply("Failed to parse JSON: %v", err)
		return
	}
	if len(export.Messages) == 0 {
		ce.Reply("No messages found in JSON")
		return
	}

	// Get the ghost for the other user in this DM
	ghost, err := ce.Bridge.GetGhostByID(ce.Ctx, ce.Portal.OtherUserID)
	if err != nil {
		ce.Reply("Failed to get ghost: %v", err)
		return
	}

	// Get double puppet intent for sending as the user
	userIntent := ce.User.DoublePuppet(ce.Ctx)
	if userIntent == nil {
		ce.Reply("Double puppet not available, can't send messages as you")
		return
	}

	ghostName := ghost.Name

	// Sort messages by timestamp ascending
	sort.Slice(export.Messages, func(i, j int) bool {
		return export.Messages[i].Timestamp < export.Messages[j].Timestamp
	})

	imported := 0
	skipped := 0
	for _, msg := range export.Messages {
		if msg.Type != "text" || msg.IsUnsent || msg.Text == "" {
			skipped++
			continue
		}

		// Pick the right intent: if sender matches ghost name, use ghost; otherwise use double puppet (the user)
		var intent bridgev2.MatrixAPI
		if msg.SenderName == ghostName {
			intent = ghost.Intent
		} else {
			intent = userIntent
		}

		content := &event.Content{
			Parsed: &event.MessageEventContent{
				MsgType: event.MsgText,
				Body:    msg.Text,
			},
		}

		ts := time.UnixMilli(msg.Timestamp)
		_, err := intent.SendMessage(ce.Ctx, ce.Portal.MXID, event.EventMessage, content, &bridgev2.MatrixSendExtra{
			Timestamp: ts,
		})
		if err != nil {
			ce.Log.Err(err).Int64("timestamp", msg.Timestamp).Msg("Failed to send imported message")
			ce.Reply("Error sending message (ts=%d): %v", msg.Timestamp, err)
			return
		}
		imported++
	}

	ce.Reply(fmt.Sprintf("Imported %d messages (%d skipped)", imported, skipped))
}
