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

	body := make(map[string]interface{})

	if from := evt.Info.SourceString(); from != "" {
		body["SenderNumber"] = from
	}

	// Adiciona informações da enquete, se for uma atualização de votação
	if pollUpdate := evt.Message.GetPollUpdateMessage(); pollUpdate != nil {
		// Log para depuração: capturar dados brutos do PollUpdateMessage
		logrus.Debugf("PollUpdateMessage received: %+v", pollUpdate)
		body["message"] = map[string]interface{}{
			"ID":            evt.Info.ID,
			"TextMessage":   message.Text,
			"RepliedId":     message.RepliedId,
			"MessageOrigin": message.QuotedMessage,
			"PollUpdate": map[string]interface{}{
				"PollID": pollUpdate.GetPollCreationMessageKey().GetID(),
				// Placeholder: opções selecionadas não implementadas devido à falta de GetSelectedOptions
				"SelectedOptions": []map[string]interface{}{},
			},
		}
	} else {
		body["message"] = map[string]interface{}{
			"ID":            message.ID,
			"TextMessage":   message.Text,
			"RepliedId":     message.RepliedId,
			"MessageOrigin": message.QuotedMessage,
		}
	}

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

	// Verifica se a mensagem é do próprio número
	waCli := GetWaCli()
	MyNumber := false
	if waCli != nil && waCli.Store.ID != nil {
		MyNumber = extractPhoneNumber(evt.Info.SourceString()) == extractPhoneNumber(waCli.Store.ID.String())
	}
	body["MyNumber"] = MyNumber

	body["Type"] = determineMessageType(evt, message.Text)

	// Adiciona a porta configurada no payload
	body["Port"] = config.AppPort

	if audioMedia := evt.Message.GetAudioMessage(); audioMedia != nil {
		path, err := ExtractMedia(ctx, config.PathMedia, audioMedia)
		if err != nil {
			logrus.Errorf("Failed to download audio: %v", err)
			return nil, pkgError.WebhookError(fmt.Sprintf("Failed to download audio: %v", err))
		}
		body["audio"] = path
	}
	if contactMessage := evt.Message.GetContactMessage(); contactMessage != nil {
		body["contact"] = contactMessage
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

// Função auxiliar para obter o título da opção da enquete
func getPollOptionTitle(ctx context.Context, evt *events.Message, option []byte) string {
	// Placeholder: Retorna o hash da opção como string.
	// Para obter o título real, implemente lógica para consultar a mensagem original da enquete.
	return fmt.Sprintf("Option_%x", option)
}

func determineMessageType(evt *events.Message, text string) string {
	if evt.Message.GetAudioMessage() != nil {
		if evt.Message.GetAudioMessage().GetPTT() {
			return "voice_message"
		}
		return "audio"
	}
	if evt.Message.GetImageMessage() != nil {
		return "image"
	}
	if evt.Message.GetVideoMessage() != nil {
		return "video"
	}
	if evt.Message.GetDocumentMessage() != nil {
		return "document"
	}
	if evt.Message.GetStickerMessage() != nil {
		return "sticker"
	}
	if evt.Message.GetContactMessage() != nil {
		return "contact"
	}
	if evt.Message.GetLocationMessage() != nil {
		return "location"
	}
	if evt.Message.GetLiveLocationMessage() != nil {
		return "live_location"
	}
	if evt.Message.GetListMessage() != nil {
		return "list"
	}
	if evt.Message.GetOrderMessage() != nil {
		return "order"
	}
	if evt.Message.GetPaymentInviteMessage() != nil {
		return "payment"
	}
	if evt.Message.GetPollCreationMessageV3() != nil || evt.Message.GetPollCreationMessageV4() != nil || evt.Message.GetPollCreationMessageV5() != nil {
		return "poll"
	}
	if evt.Message.GetPollUpdateMessage() != nil {
		return "poll"
	}
	if evt.Message.GetReactionMessage() != nil {
		return "reaction"
	}
	if evt.Message.GetConversation() != "" || evt.Message.GetExtendedTextMessage() != nil {
		urlRegex := regexp.MustCompile(`https?://[^\s]+`)
		if urlRegex.MatchString(text) {
			return "link"
		}
		return "text"
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
