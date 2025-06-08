package whatsapp

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"mime"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/aldinokemal/go-whatsapp-web-multidevice/config"
	pkgError "github.com/aldinokemal/go-whatsapp-web-multidevice/pkg/error"
)

func ExtractMedia(ctx context.Context, storageLocation string, mediaFile whatsmeow.DownloadableMessage) (ExtractedMedia, error) {
	var extractedMedia ExtractedMedia
	if mediaFile == nil {
		logrus.Info("Skip download because data is nil")
		return extractedMedia, nil
	}

	waCli := GetWaCli()
	data, err := waCli.Download(ctx, mediaFile)
	if err != nil {
		return extractedMedia, err
	}

	maxFileSize := config.WhatsappSettingMaxDownloadSize
	if int64(len(data)) > maxFileSize {
		return extractedMedia, fmt.Errorf("file size exceeds the maximum limit of %d bytes", maxFileSize)
	}

	switch media := mediaFile.(type) {
	case *waProto.ImageMessage:
		extractedMedia.MimeType = media.GetMimetype()
		extractedMedia.Caption = media.GetCaption()
	case *waProto.AudioMessage:
		extractedMedia.MimeType = media.GetMimetype()
	case *waProto.VideoMessage:
		extractedMedia.MimeType = media.GetMimetype()
		extractedMedia.Caption = media.GetCaption()
	case *waProto.StickerMessage:
		extractedMedia.MimeType = media.GetMimetype()
	case *waProto.DocumentMessage:
		extractedMedia.MimeType = media.GetMimetype()
		extractedMedia.Caption = media.GetCaption()
	}

	var extension string
	if ext, err := mime.ExtensionsByType(extractedMedia.MimeType); err == nil && len(ext) > 0 {
		extension = ext[0]
	} else if parts := strings.Split(extractedMedia.MimeType, "/"); len(parts) > 1 {
		extension = "." + parts[len(parts)-1]
	}

	extractedMedia.MediaPath = fmt.Sprintf("%s/%d-%s%s", storageLocation, time.Now().Unix(), uuid.NewString(), extension)
	err = os.WriteFile(extractedMedia.MediaPath, data, 0600)
	if err != nil {
		return extractedMedia, err
	}
	return extractedMedia, nil
}

func SanitizePhone(phone *string) {
	if phone != nil && len(*phone) > 0 && !strings.Contains(*phone, "@") {
		if len(*phone) <= 15 {
			*phone = fmt.Sprintf("%s%s", *phone, config.WhatsappTypeUser)
		} else {
			*phone = fmt.Sprintf("%s%s", *phone, config.WhatsappTypeGroup)
		}
	}
}

func GetPlatformName(deviceID int) string {
	switch deviceID {
	case 0:
		return "UNKNOWN"
	case 1:
		return "CHROME"
	case 2:
		return "FIREFOX"
	case 3:
		return "IE"
	case 4:
		return "OPERA"
	case 5:
		return "SAFARI"
	case 6:
		return "EDGE"
	case 7:
		return "DESKTOP"
	case 8:
		return "IPAD"
	case 9:
		return "ANDROID_TABLET"
	case 10:
		return "OHANA"
	case 11:
		return "ALOHA"
	case 12:
		return "CATALINA"
	case 13:
		return "TCL_TV"
	default:
		return "UNKNOWN"
	}
}

func ParseJID(arg string) (types.JID, error) {
	if arg[0] == '+' {
		arg = arg[1:]
	}
	if !strings.ContainsRune(arg, '@') {
		return types.NewJID(arg, types.DefaultUserServer), nil
	}

	recipient, err := types.ParseJID(arg)
	if err != nil {
		return recipient, pkgError.ErrInvalidJID
	}

	if recipient.User == "" {
		return recipient, pkgError.ErrInvalidJID
	}
	return recipient, nil
}

func IsOnWhatsapp(waCli *whatsmeow.Client, jid string) bool {
	if strings.Contains(jid, "@s.whatsapp.net") {
		data, err := waCli.IsOnWhatsApp([]string{jid})
		if err != nil {
			logrus.Error("Failed to check if user is on WhatsApp: ", err)
			return false
		}
		for _, v := range data {
			logrus.Info("User ", jid, " is on WhatsApp: ", v.IsIn)
			if !v.IsIn {
				return false
			}
		}
	}
	return true
}

func ValidateJidWithLogin(waCli *whatsmeow.Client, jid string) (types.JID, error) {
	MustLogin(waCli)
	if config.WhatsappAccountValidation && !IsOnWhatsapp(waCli, jid) {
		return types.JID{}, pkgError.InvalidJID(fmt.Sprintf("Phone %s is not on WhatsApp", jid))
	}
	return ParseJID(jid)
}

func MustLogin(waCli *whatsmeow.Client) {
	if waCli == nil {
		panic(pkgError.InternalServerError("WhatsApp client is not initialized"))
	}
	if !waCli.IsConnected() {
		panic(pkgError.ErrNotConnected)
	} else if !waCli.IsLoggedIn() {
		panic(pkgError.ErrNotLoggedIn)
	}
}

func FormatJID(jid string) types.JID {
	if idx := strings.LastIndex(jid, ":"); idx != -1 && strings.Contains(jid, "@s.whatsapp.net") {
		jid = jid[:idx] + jid[strings.Index(jid, "@s.whatsapp.net"):]
	}
	formattedJID, err := ParseJID(jid)
	if err != nil {
		return types.JID{}
	}
	return formattedJID
}

func isGroupJid(jid string) bool {
	return strings.Contains(jid, "@g.us")
}

func isFromMySelf(jid string) bool {
	waCli := GetWaCli()
	return extractPhoneNumber(jid) == extractPhoneNumber(waCli.Store.ID.String())
}

func extractPhoneNumber(jid string) string {
	regex := regexp.MustCompile(`\d+`)
	matches := regex.FindAllString(jid, -1)
	if len(matches) > 0 {
		return matches[0]
	}
	return ""
}

func getMessageDigestOrSignature(msg, key []byte) (string, error) {
	mac := hmac.New(sha256.New, key)
	_, err := mac.Write(msg)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(mac.Sum(nil)), nil
}

func ExtractMessageText(evt *events.Message) string {
	messageText := evt.Message.GetConversation()
	if extendedText := evt.Message.GetExtendedTextMessage(); extendedText != nil {
		messageText = extendedText.GetText()
	} else if protocolMessage := evt.Message.GetProtocolMessage(); protocolMessage != nil {
		if editedMessage := protocolMessage.GetEditedMessage(); editedMessage != nil {
			if extendedText := editedMessage.GetExtendedTextMessage(); extendedText != nil {
				messageText = extendedText.GetText()
			}
		}
	} else if imageMessage := evt.Message.GetImageMessage(); imageMessage != nil {
		messageText = imageMessage.GetCaption()
		if messageText == "" {
			messageText = "🖼️ Image"
		} else {
			messageText = "🖼️ " + messageText
		}
	} else if documentMessage := evt.Message.GetDocumentMessage(); documentMessage != nil {
		messageText = documentMessage.GetCaption()
		if messageText == "" {
			messageText = "📄 Document"
		} else {
			messageText = "📄 " + messageText
		}
	} else if videoMessage := evt.Message.GetVideoMessage(); videoMessage != nil {
		messageText = videoMessage.GetCaption()
		if messageText == "" {
			messageText = "🎥 Video"
		} else {
			messageText = "🎥 " + messageText
		}
	} else if liveLocationMessage := evt.Message.GetLiveLocationMessage(); liveLocationMessage != nil {
		messageText = "📍 Live Location"
	} else if locationMessage := evt.Message.GetLocationMessage(); locationMessage != nil {
		messageText = "📍 Location"
	} else if stickerMessage := evt.Message.GetStickerMessage(); stickerMessage != nil {
		messageText = "🎨 Sticker"
		if stickerMessage.GetIsAnimated() {
			messageText = "✨ Animated Sticker"
		}
		if stickerMessage.GetAccessibilityLabel() != "" {
			messageText += " - " + stickerMessage.GetAccessibilityLabel()
		}
	} else if contactMessage := evt.Message.GetContactMessage(); contactMessage != nil {
		messageText = contactMessage.GetDisplayName()
		if messageText == "" {
			messageText = "👤 Contact"
		} else {
			messageText = "👤 " + messageText
		}
	} else if listMessage := evt.Message.GetListMessage(); listMessage != nil {
		messageText = listMessage.GetTitle()
		if messageText == "" {
			messageText = "📝 List"
		} else {
			messageText = "📝 " + messageText
		}
	} else if orderMessage := evt.Message.GetOrderMessage(); orderMessage != nil {
		messageText = orderMessage.GetOrderTitle()
		if messageText == "" {
			messageText = "🛍️ Order"
		} else {
			messageText = "🛍️ " + messageText
		}
	} else if paymentMessage := evt.Message.GetPaymentInviteMessage(); paymentMessage != nil {
		messageText = paymentMessage.GetServiceType().String()
		if messageText == "" {
			messageText = "💳 Payment"
		} else {
			messageText = "💳 " + messageText
		}
	} else if audioMessage := evt.Message.GetAudioMessage(); audioMessage != nil {
		messageText = "🎧 Audio"
		if audioMessage.GetPTT() {
			messageText = "🎤 Voice Message"
		}
	} else if pollMessageV3 := evt.Message.GetPollCreationMessageV3(); pollMessageV3 != nil {
		messageText = pollMessageV3.GetName()
		if messageText == "" {
			messageText = "📊 Poll"
		} else {
			messageText = "📊 " + messageText
		}
	} else if pollMessageV4 := evt.Message.GetPollCreationMessageV4(); pollMessageV4 != nil {
		messageText = pollMessageV4.GetMessage().GetConversation()
		if messageText == "" {
			messageText = "📊 Poll"
		} else {
			messageText = "📊 " + messageText
		}
	} else if pollMessageV5 := evt.Message.GetPollCreationMessageV5(); pollMessageV5 != nil {
		messageText = pollMessageV5.GetMessage().GetConversation()
		if messageText == "" {
			messageText = "📊 Poll"
		} else {
			messageText = "📊 " + messageText
		}
	}
	return messageText
}
