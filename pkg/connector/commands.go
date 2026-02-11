package connector

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

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
	Participants []string           `json:"participants"`
	Messages     []messengerMessage `json:"messages"`
}

type messengerMessage struct {
	SenderName  string `json:"senderName"`
	Text        string `json:"text"`
	Timestamp   int64  `json:"timestamp"`
	Type        string `json:"type"`
	IsUnsent    bool   `json:"isUnsent"`
}

func downloadJSON(ce *commands.Event) ([]byte, bool) {
	if ce.ReplyTo != "" {
		evt, err := ce.Bot.GetEvent(ce.Ctx, ce.RoomID, ce.ReplyTo)
		if err != nil {
			ce.Reply("Failed to get replied-to event: %v", err)
			return nil, false
		}
		content, ok := evt.Content.Parsed.(*event.MessageEventContent)
		if !ok {
			ce.Reply("Replied-to event is not a message")
			return nil, false
		}
		url := content.URL
		var file *event.EncryptedFileInfo
		if content.File != nil {
			url = content.File.URL
			file = content.File
		}
		if url == "" {
			ce.Reply("Replied-to message has no file attachment")
			return nil, false
		}
		data, err := ce.Bot.DownloadMedia(ce.Ctx, url, file)
		if err != nil {
			ce.Reply("Failed to download file: %v", err)
			return nil, false
		}
		return data, true
	}

	// Find mxc:// URL in args (skip --dry-run)
	for _, arg := range ce.Args {
		if strings.HasPrefix(arg, "mxc://") {
			data, err := ce.Bot.DownloadMedia(ce.Ctx, id.ContentURIString(arg), nil)
			if err != nil {
				ce.Reply("Failed to download file: %v", err)
				return nil, false
			}
			return data, true
		}
	}

	ce.Reply("Usage: upload JSON file to this room, then reply to it with `import-messages`")
	return nil, false
}

func fnImportMessages(ce *commands.Event) {
	if ce.Portal.RoomType != database.RoomTypeDM {
		ce.Reply("This command only works in DM rooms")
		return
	}

	dryRun := false
	for _, arg := range ce.Args {
		if arg == "--dry-run" {
			dryRun = true
			break
		}
	}

	jsonData, ok := downloadJSON(ce)
	if !ok {
		return
	}

	export, err := parseMessengerExport(jsonData)
	if err != nil {
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

	ghostName := ghost.Name
	toSend := filterAndSortMessages(export.Messages)

	if dryRun {
		ghostCount := 0
		userCount := 0
		for _, msg := range toSend {
			if msg.SenderName == ghostName {
				ghostCount++
			} else {
				userCount++
			}
		}
		first := time.UnixMilli(toSend[0].Timestamp).Format("2006-01-02 15:04")
		last := time.UnixMilli(toSend[len(toSend)-1].Timestamp).Format("2006-01-02 15:04")
		ce.Reply("Dry run: %d messages to import (%d skipped), %d as ghost (%s), %d as you, from %s to %s",
			len(toSend), len(export.Messages)-len(toSend), ghostCount, ghostName, userCount, first, last)
		return
	}

	// Get double puppet intent for sending as the user
	userIntent := ce.User.DoublePuppet(ce.Ctx)
	if userIntent == nil {
		ce.Reply("Double puppet not available, can't send messages as you")
		return
	}

	for _, msg := range toSend {
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
	}

	ce.Reply(fmt.Sprintf("Imported %d messages (%d skipped)", len(toSend), len(export.Messages)-len(toSend)))
}

func parseMessengerExport(data []byte) (*messengerExport, error) {
	var export messengerExport
	if err := json.Unmarshal(data, &export); err != nil {
		return nil, err
	}
	return &export, nil
}

func filterAndSortMessages(msgs []messengerMessage) []messengerMessage {
	var out []messengerMessage
	for _, msg := range msgs {
		if msg.Type == "text" && !msg.IsUnsent && msg.Text != "" {
			out = append(out, msg)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Timestamp < out[j].Timestamp
	})
	return out
}
