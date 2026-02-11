package connector

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
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

// --- Message import types and state ---

type messengerExport struct {
	Participants []string           `json:"participants"`
	Messages     []messengerMessage `json:"messages"`
}

type messengerMessage struct {
	SenderName string           `json:"senderName"`
	Text       string           `json:"text"`
	Timestamp  int64            `json:"timestamp"`
	Type       string           `json:"type"`
	IsUnsent   bool             `json:"isUnsent"`
	Media      []messengerMedia `json:"media"`
}

type messengerMedia struct {
	URI string `json:"uri"`
}

// loadedConversation holds a parsed and filtered conversation ready for import.
type loadedConversation struct {
	OtherParticipant string
	AllMessages      []messengerMessage
	ToSend           []messengerMessage
}

// sentEvents tracks event IDs sent per room, for undo.
type sentEvents struct {
	RoomID   id.RoomID
	EventIDs []id.EventID
}

var (
	loadedConversations   map[string]*loadedConversation // keyed by other participant name (lowercased)
	loadedMediaFiles      map[string][]byte              // keyed by path (e.g. "media/uuid.jpeg")
	lastSentEvents        *sentEvents
	loadedConversationsMu sync.Mutex
)

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
		if msg.IsUnsent {
			continue
		}
		switch msg.Type {
		case "text", "link":
			if msg.Text != "" {
				out = append(out, msg)
			}
		case "media":
			if len(msg.Media) > 0 {
				out = append(out, msg)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Timestamp < out[j].Timestamp
	})
	return out
}

// otherParticipant returns the participant name that isn't "Piotr Gwiazdowski".
// For DM exports, there are exactly 2 participants.
func otherParticipant(participants []string) string {
	for _, p := range participants {
		if p != "Piotr Gwiazdowski" {
			return p
		}
	}
	return ""
}

func downloadAttachment(ce *commands.Event) ([]byte, bool) {
	if ce.ReplyTo == "" {
		ce.Reply("Reply to a file message with this command")
		return nil, false
	}
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

var cmdExport = &commands.FullHandler{
	Func: fnExport,
	Name: "export",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionChats,
		Description: "Manage Messenger JSON exports: load (reply to zip), list, send",
	},
	RequiresLogin: true,
}

func fnExport(ce *commands.Event) {
	if len(ce.Args) == 0 {
		ce.Reply("Usage: `export load` (reply to zip), `export list`, `export send` (in DM room)")
		return
	}
	switch ce.Args[0] {
	case "load":
		fnExportLoad(ce)
	case "list":
		fnExportList(ce)
	case "send":
		fnExportSend(ce)
	case "undo":
		fnExportUndo(ce)
	default:
		ce.Reply("Unknown subcommand %q. Use: load, list, send, undo", ce.Args[0])
	}
}

func fnExportLoad(ce *commands.Event) {
	zipData, ok := downloadAttachment(ce)
	if !ok {
		return
	}

	reader, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		ce.Reply("Failed to read zip: %v", err)
		return
	}

	convos := make(map[string]*loadedConversation)
	mediaFiles := make(map[string][]byte)
	var errors []string

	// First pass: read all files from the zip
	zipFiles := make(map[string][]byte)
	for _, f := range reader.File {
		if f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			errors = append(errors, fmt.Sprintf("%s: open error: %v", f.Name, err))
			continue
		}
		var buf bytes.Buffer
		_, err = buf.ReadFrom(rc)
		rc.Close()
		if err != nil {
			errors = append(errors, fmt.Sprintf("%s: read error: %v", f.Name, err))
			continue
		}
		zipFiles[f.Name] = buf.Bytes()
	}

	// Store media files (anything not .json)
	for name, data := range zipFiles {
		if filepath.Ext(name) != ".json" {
			mediaFiles[name] = data
		}
	}

	// Parse JSON files
	for name, data := range zipFiles {
		if filepath.Ext(name) != ".json" {
			continue
		}

		export, err := parseMessengerExport(data)
		if err != nil {
			errors = append(errors, fmt.Sprintf("%s: parse error: %v", name, err))
			continue
		}

		other := otherParticipant(export.Participants)
		if other == "" {
			errors = append(errors, fmt.Sprintf("%s: can't determine other participant from %v", name, export.Participants))
			continue
		}

		toSend := filterAndSortMessages(export.Messages)
		key := strings.ToLower(other)
		convos[key] = &loadedConversation{
			OtherParticipant: other,
			AllMessages:      export.Messages,
			ToSend:           toSend,
		}
	}

	loadedConversationsMu.Lock()
	loadedConversations = convos
	loadedMediaFiles = mediaFiles
	loadedConversationsMu.Unlock()

	var sb strings.Builder
	fmt.Fprintf(&sb, "Loaded %d conversations, %d media files:\n\n", len(convos), len(mediaFiles))
	for _, c := range convos {
		fmt.Fprintf(&sb, "- **%s**: %d importable, %d skipped\n",
			c.OtherParticipant, len(c.ToSend), len(c.AllMessages)-len(c.ToSend))
	}
	if len(errors) > 0 {
		fmt.Fprintf(&sb, "\nErrors:\n")
		for _, e := range errors {
			fmt.Fprintf(&sb, "- %s\n", e)
		}
	}
	ce.Reply(sb.String())
}

func fnExportList(ce *commands.Event) {
	loadedConversationsMu.Lock()
	convos := loadedConversations
	loadedConversationsMu.Unlock()

	if len(convos) == 0 {
		ce.Reply("No conversations loaded. Use `export load` first (reply to a zip file).")
		return
	}

	var sb strings.Builder
	totalImportable := 0
	for _, c := range convos {
		n := len(c.ToSend)
		totalImportable += n
		if n == 0 {
			fmt.Fprintf(&sb, "- **%s**: no importable messages\n", c.OtherParticipant)
			continue
		}
		first := time.UnixMilli(c.ToSend[0].Timestamp).Format("2006-01-02 15:04")
		last := time.UnixMilli(c.ToSend[n-1].Timestamp).Format("2006-01-02 15:04")
		fmt.Fprintf(&sb, "- **%s**: %d messages (%d skipped), %s to %s\n",
			c.OtherParticipant, n, len(c.AllMessages)-n, first, last)
	}
	fmt.Fprintf(&sb, "\nTotal: %d importable messages across %d conversations", totalImportable, len(convos))
	ce.Reply(sb.String())
}

func fnExportSend(ce *commands.Event) {
	if ce.Portal == nil || ce.Portal.RoomType != database.RoomTypeDM {
		ce.Reply("Run this in a DM portal room")
		return
	}

	loadedConversationsMu.Lock()
	convos := loadedConversations
	loadedConversationsMu.Unlock()

	if len(convos) == 0 {
		ce.Reply("No conversations loaded. Use `export load` first.")
		return
	}

	// Get the ghost for the other user in this DM
	ghost, err := ce.Bridge.GetGhostByID(ce.Ctx, ce.Portal.OtherUserID)
	if err != nil {
		ce.Reply("Failed to get ghost: %v", err)
		return
	}

	// Match loaded conversation by ghost name
	key := strings.ToLower(ghost.Name)
	convo, ok := convos[key]
	if !ok {
		ce.Reply("No loaded conversation for %q. Available: ", ghost.Name)
		var names []string
		for _, c := range convos {
			names = append(names, c.OtherParticipant)
		}
		ce.Reply("No loaded conversation for %q. Available: %s", ghost.Name, strings.Join(names, ", "))
		return
	}

	if len(convo.ToSend) == 0 {
		ce.Reply("No importable messages for %s", convo.OtherParticipant)
		return
	}

	// Get double puppet intent for sending as the user
	userIntent := ce.User.DoublePuppet(ce.Ctx)
	if userIntent == nil {
		ce.Reply("Double puppet not available, can't send messages as you")
		return
	}

	loadedConversationsMu.Lock()
	mediaFiles := loadedMediaFiles
	loadedConversationsMu.Unlock()

	ghostName := ghost.Name
	var eventIDs []id.EventID

	for _, msg := range convo.ToSend {
		var intent bridgev2.MatrixAPI
		if msg.SenderName == ghostName {
			intent = ghost.Intent
		} else {
			intent = userIntent
		}

		ts := time.UnixMilli(msg.Timestamp)

		if msg.Type == "media" && len(msg.Media) > 0 {
			for _, m := range msg.Media {
				mediaPath := strings.TrimPrefix(m.URI, "./")
				data, ok := mediaFiles[mediaPath]
				if !ok {
					ce.Log.Warn().Str("path", mediaPath).Msg("Media file not found in zip")
					continue
				}

				ext := strings.ToLower(filepath.Ext(mediaPath))
				var mimeType string
				var msgType event.MessageType
				switch ext {
				case ".jpeg", ".jpg":
					mimeType = "image/jpeg"
					msgType = event.MsgImage
				case ".png":
					mimeType = "image/png"
					msgType = event.MsgImage
				case ".gif":
					mimeType = "image/gif"
					msgType = event.MsgImage
				case ".mp4":
					mimeType = "video/mp4"
					msgType = event.MsgVideo
				case ".webm":
					mimeType = "video/webm"
					msgType = event.MsgVideo
				default:
					mimeType = "application/octet-stream"
					msgType = event.MsgFile
				}

				fileName := filepath.Base(mediaPath)
				mxcURL, encFile, err := intent.UploadMedia(ce.Ctx, ce.Portal.MXID, data, fileName, mimeType)
				if err != nil {
					ce.Log.Err(err).Str("path", mediaPath).Msg("Failed to upload media")
					ce.Reply("Error uploading %s: %v. Use `export undo` to redact %d sent so far.", mediaPath, err, len(eventIDs))
					goto done
				}

				resp, err := intent.SendMessage(ce.Ctx, ce.Portal.MXID, event.EventMessage, &event.Content{
					Parsed: &event.MessageEventContent{
						MsgType: msgType,
						Body:    fileName,
						URL:     mxcURL,
						File:    encFile,
						Info: &event.FileInfo{
							MimeType: mimeType,
							Size:     len(data),
						},
					},
				}, &bridgev2.MatrixSendExtra{
					Timestamp: ts,
				})
				if err != nil {
					ce.Log.Err(err).Int64("timestamp", msg.Timestamp).Msg("Failed to send media message")
					ce.Reply("Error sending media (ts=%d): %v. Use `export undo` to redact %d sent so far.", msg.Timestamp, err, len(eventIDs))
					goto done
				}
				eventIDs = append(eventIDs, resp.EventID)
			}
		} else {
			resp, err := intent.SendMessage(ce.Ctx, ce.Portal.MXID, event.EventMessage, &event.Content{
				Parsed: &event.MessageEventContent{
					MsgType: event.MsgText,
					Body:    msg.Text,
				},
			}, &bridgev2.MatrixSendExtra{
				Timestamp: ts,
			})
			if err != nil {
				ce.Log.Err(err).Int64("timestamp", msg.Timestamp).Msg("Failed to send imported message")
				ce.Reply("Error sending message (ts=%d): %v. Use `export undo` to redact %d sent so far.", msg.Timestamp, err, len(eventIDs))
				goto done
			}
			eventIDs = append(eventIDs, resp.EventID)
		}
	}

	ce.Reply(fmt.Sprintf("Imported %d messages for %s (%d skipped). Use `export undo` to revert.",
		len(eventIDs), convo.OtherParticipant, len(convo.AllMessages)-len(convo.ToSend)))

done:
	loadedConversationsMu.Lock()
	lastSentEvents = &sentEvents{RoomID: ce.Portal.MXID, EventIDs: eventIDs}
	loadedConversationsMu.Unlock()
}

func fnExportUndo(ce *commands.Event) {
	loadedConversationsMu.Lock()
	sent := lastSentEvents
	lastSentEvents = nil
	loadedConversationsMu.Unlock()

	if sent == nil || len(sent.EventIDs) == 0 {
		ce.Reply("Nothing to undo")
		return
	}

	ce.Reply("Redacting %d messages...", len(sent.EventIDs))

	failed := 0
	for _, evtID := range sent.EventIDs {
		_, err := ce.Bot.SendMessage(ce.Ctx, sent.RoomID, event.EventRedaction, &event.Content{
			Parsed: &event.RedactionEventContent{
				Redacts: evtID,
			},
		}, nil)
		if err != nil {
			ce.Log.Err(err).Str("event_id", evtID.String()).Msg("Failed to redact")
			failed++
		}
	}

	if failed > 0 {
		ce.Reply("Redacted %d messages (%d failed)", len(sent.EventIDs)-failed, failed)
	} else {
		ce.Reply("Redacted all %d messages", len(sent.EventIDs))
	}
}
