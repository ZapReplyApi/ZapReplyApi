package whatsapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/aldinokemal/go-whatsapp-web-multidevice/config"
	pkgError "github.com/aldinokemal/go-whatsapp-web-multidevice/pkg/error"
	"github.com/sirupsen/logrus"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func forwardToWebhook(ctx context.Context, evt *events.Message) error {
	logrus.Info("Forwarding event to webhook:", config.WhatsappWebhook)
	payload, err := createPayload(ctx, evt)
	if err != nil {
		return err
	}

	for _, url := range config.WhatsappWebhook {
		if err = SubmitWebhook(payload, url); err != nil {
			return err
		}
	}

	logrus.Info("Event forwarded to webhook")
	return nil
}

func createPayload(ctx context.Context, evt *events.Message) (map[string]interface{}, error) {
	message := buildEventMessage(evt)
	waReaction := buildEventReaction(evt)
	forwarded := buildForwarded(evt)

	// Logar mensagem bruta para debug
	logrus.Debugf("Raw message: %+v", evt.Message)

	body := make(map[string]interface{})

	if from := evt.Info.SourceString(); from != "" {
		body["SenderNumber"] = from
	}

	messageData := make(map[string]interface{})
	// Definir campos na ordem desejada
	messageData["ID"] = message.ID
	messageData["MessageOrigin"] = message.QuotedMessage
	messageData["RepliedId"] = message.RepliedId

	// Verificar se é uma mensagem com link (extendedTextMessage)
	if extendedText := evt.Message.GetExtendedTextMessage(); extendedText != nil {
		urlRegex := regexp.MustCompile(`https?://[^\s]+`)
		if urlRegex.MatchString(extendedText.GetText()) {
			if title := extendedText.GetTitle(); title != "" {
				messageData["TitleLink"] = title
			}
			if description := extendedText.GetDescription(); description != "" {
				messageData["LinkDescription"] = description
			}
			messageData["TextMessage"] = extendedText.GetText()
		} else {
			messageData["TextMessage"] = message.Text
		}
	} else {
		messageData["TextMessage"] = message.Text
	}

	if pollUpdate := evt.Message.GetPollUpdateMessage(); pollUpdate != nil {
		logrus.Debugf("PollUpdateMessage received: %+v", pollUpdate)
		messageData["PollUpdate"] = map[string]interface{}{
			"PollID": pollUpdate.GetPollCreationMessageKey().GetID(),
			"SelectedOptions": []map[string]interface{}{},
		}
	}

	body["message"] = messageData

	if pushname := evt.Info.PushName; pushname != "" {
		body["PushName"] = pushname
	}
	if waReaction.Message != "" {
		body["reaction"] = waReaction
	}
	if evt.IsViewOnce {
		body["view_once"] = evt.IsViewOnce
	}
	if forwarded {
		body["forwarded"] = forwarded
	}
	if timestamp := evt.Info.Timestamp.Format(time.RFC3339); timestamp != "" {
		body["timestamp"] = timestamp
	}

	jid, err := types.ParseJID(evt.Info.Chat.String())
	if err != nil {
		return nil, pkgError.WebhookError(fmt.Sprintf("Invalid JID: %v", err))
	}
	IsGroup := strings.Contains(evt.Info.Chat.String(), "@g.us")
	body["IsGroup"] = IsGroup
	if IsGroup {
		GroupName, err := GetGroupName(ctx, jid)
		if err != nil {
			logrus.Errorf("Failed to get group name: %v", err)
		} else if GroupName != "" {
			body["GroupName"] = GroupName
		}
	}

	waCli := GetWaCli()
	MyNumber := false
	if waCli != nil && waCli.Store.ID != nil {
		MyNumber = extractPhoneNumber(evt.Info.SourceString()) == extractPhoneNumber(waCli.Store.ID.String())
	}
	body["MyNumber"] = MyNumber

	body["Type"] = determineMessageType(evt, message.Text)

	body["Port"] = config.AppPort

	if contactMessage := evt.Message.GetContactMessage(); contactMessage != nil {
		logrus.Debugf("Single ContactMessage detected: %+v", contactMessage)
		body["contact"] = []interface{}{
			map[string]interface{}{
				"displayName": contactMessage.GetDisplayName(),
				"vcard":       contactMessage.GetVcard(),
			},
		}
	}

	if evt.Info.Type == "media" && strings.Contains(fmt.Sprintf("%+v", evt.Message), "contactsArrayMessage") {
		logrus.Debugf("Multiple contacts message detected in media type: %+v", evt.Message)
		rawMessage := fmt.Sprintf("%+v", evt.Message)
		contacts := []interface{}{}
		re := regexp.MustCompile(`contacts:{displayName:"(.*?)".*?vcard:"(.*?)"}`)
		matches := re.FindAllStringSubmatch(rawMessage, -1)
		logrus.Debugf("Regex matches found: %d", len(matches))
		for i, match := range matches {
			if len(match) == 3 {
				vcard := strings.ReplaceAll(match[2], `\n`, "\n")
				contacts = append(contacts, map[string]interface{}{
					"displayName": match[1],
					"vcard":       vcard,
				})
			} else {
				logrus.Warnf("Invalid match at index %d: %v", i, match)
			}
		}
		body["contact"] = contacts
		body["Type"] = "contact_message"
		logrus.Warnf("Extracted %d contacts from raw message data: %+v", len(contacts), contacts)
	}

	if audioMedia := evt.Message.GetAudioMessage(); audioMedia != nil {
		path, err := ExtractMedia(ctx, config.PathMedia, audioMedia)
		if err != nil {
			logrus.Errorf("Failed to download audio: %v", err)
			return nil, pkgError.WebhookError(fmt.Sprintf("Failed to download audio: %v", err))
		}
		body["audio"] = path
	}
	if documentMessage := evt.Message.GetDocumentMessage(); documentMessage != nil {
		path, err := ExtractMedia(ctx, config.PathMedia, documentMessage)
		if err != nil {
			logrus.Errorf("Failed to download document: %v", err)
			return nil, pkgError.WebhookError(fmt.Sprintf("Failed to download document: %v", err))
		}
		body["document"] = path
	}
	if imageMedia := evt.Message.GetImageMessage(); imageMedia != nil {
		path, err := ExtractMedia(ctx, config.PathMedia, imageMedia)
		if err != nil {
			logrus.Errorf("Failed to download image: %v", err)
			return nil, pkgError.WebhookError(fmt.Sprintf("Failed to download image: %v", err))
		}
		body["image"] = path
	}
	if listMessage := evt.Message.GetListMessage(); listMessage != nil {
		body["list"] = listMessage
	}
	if liveLocationMessage := evt.Message.GetLiveLocationMessage(); liveLocationMessage != nil {
		body["live_location"] = liveLocationMessage
	}
	if locationMessage := evt.Message.GetLocationMessage(); locationMessage != nil {
		body["location"] = locationMessage
	}
	if orderMessage := evt.Message.GetOrderMessage(); orderMessage != nil {
		body["order"] = orderMessage
	}
	if stickerMedia := evt.Message.GetStickerMessage(); stickerMedia != nil {
		path, err := ExtractMedia(ctx, config.PathMedia, stickerMedia)
		if err != nil {
			logrus.Errorf("Failed to download sticker: %v", err)
			return nil, pkgError.WebhookError(fmt.Sprintf("Failed to download sticker: %v", err))
		}
		body["sticker"] = path
	}
	if videoMedia := evt.Message.GetVideoMessage(); videoMedia != nil {
		path, err := ExtractMedia(ctx, config.PathMedia, videoMedia)
		if err != nil {
			logrus.Errorf("Failed to download video: %v", err)
			return nil, pkgError.WebhookError(fmt.Sprintf("Failed to download video: %v", err))
		}
		body["video"] = path
	}

	return body, nil
}

func getPollOptionTitle(ctx context.Context, evt *events.Message, option []byte) string {
	return fmt.Sprintf("Option_%x", option)
}

func determineMessageType(evt *events.Message, text string) string {
	if evt.Message.GetAudioMessage() != nil {
		if evt.Message.GetAudioMessage().GetPTT() {
			return "voice_message"
		}
		return "audio_message"
	}
	if evt.Message.GetImageMessage() != nil {
		return "image_message"
	}
	if evt.Message.GetVideoMessage() != nil {
		return "video_message"
	}
	if evt.Message.GetDocumentMessage() != nil {
		return "document_message"
	}
	if evt.Message.GetStickerMessage() != nil {
		return "sticker_message"
	}
	if evt.Message.GetContactMessage() != nil {
		return "contact_message"
	}
	if evt.Info.Type == "media" && strings.Contains(fmt.Sprintf("%+v", evt.Message), "contactsArrayMessage") {
		return "contact_message"
	}
	if evt.Message.GetLocationMessage() != nil {
		return "location_message"
	}
	if evt.Message.GetLiveLocationMessage() != nil {
		return "live_location_message"
	}
	if evt.Message.GetListMessage() != nil {
		return "list_message"
	}
	if evt.Message.GetOrderMessage() != nil {
		return "order"
	}
	if evt.Message.GetPaymentInviteMessage() != nil {
		return "payment"
	}
	if evt.Message.GetPollCreationMessageV3() != nil || evt.Message.GetPollCreationMessageV4() != nil || evt.Message.GetPollCreationMessageV5() != nil {
		return "poll_message"
	}
	if evt.Message.GetPollUpdateMessage() != nil {
		return "poll_message"
	}
	if evt.Message.GetReactionMessage() != nil {
		return "reaction_message"
	}
	if evt.Message.GetConversation() != "" || evt.Message.GetExtendedTextMessage() != nil {
		urlRegex := regexp.MustCompile(`https?://[^\s]+`)
		if urlRegex.MatchString(text) {
			return "link_message"
		}
		return "text_message"
	}
	return "unknown"
}

func SubmitWebhook(payload map[string]interface{}, url string) error {
	client := &http.Client{Timeout: 10 * time.Second}

	postBody, err := json.Marshal(payload)
	if err != nil {
		return pkgError.WebhookError(fmt.Sprintf("Failed to marshal body: %v", err))
	}

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewBuffer(postBody))
	if err != nil {
		return pkgError.WebhookError(fmt.Sprintf("Error when creating HTTP request: %v", err))
	}

	secretKey := []byte(config.WhatsappWebhookSecret)
	signature, err := getMessageDigestOrSignature(postBody, secretKey)
	if err != nil {
		return pkgError.WebhookError(fmt.Sprintf("Error when creating signature: %v", err))
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", fmt.Sprintf("sha256=%s", signature))

	var attempt int
	var maxAttempts = 5
	var sleepDuration = 1 * time.Second

	for attempt = 0; attempt < maxAttempts; attempt++ {
		if _, err = client.Do(req); err == nil {
			logrus.Infof("Successfully submitted webhook on attempt %d", attempt+1)
			return nil
		}
		logrus.Warnf("Attempt %d to submit webhook failed: %v", attempt+1, err)
		time.Sleep(sleepDuration)
		sleepDuration *= 2
	}

	return pkgError.WebhookError(fmt.Sprintf("Failed after %d attempts: %v", attempt, err))
}
