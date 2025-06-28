package cmd

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aldinokemal/go-whatsapp-web-multidevice/config"
	"github.com/aldinokemal/go-whatsapp-web-multidevice/infrastructure/whatsapp"
	"github.com/aldinokemal/go-whatsapp-web-multidevice/ui/rest"
	"github.com/aldinokemal/go-whatsapp-web-multidevice/ui/rest/helpers"
	"github.com/aldinokemal/go-whatsapp-web-multidevice/ui/rest/middleware"
	"github.com/aldinokemal/go-whatsapp-web-multidevice/ui/websocket"
	"github.com/dustin/go-humanize"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/basicauth"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/gofiber/fiber/v2/middleware/filesystem"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/gofiber/template/html/v2"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	waProto "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

var restCmd = &cobra.Command{
	Use:   "rest",
	Short: "Send whatsapp API over http",
	Long:  `This application is from clone https://github.com/aldinokemal/go-whatsapp-web-multidevice`,
	Run:   restServer,
}

func init() {
	rootCmd.AddCommand(restCmd)
}

var (
	// Cache para evitar múltiplos webhooks para o mesmo call_id e Phone
	callWebhookCache = sync.Map{}
	cacheTTL         = 5 * time.Minute
)

func restServer(_ *cobra.Command, _ []string) {
	err := os.MkdirAll(config.PathQrCode, 0755)
	if err != nil {
		log.Fatalln(err)
	}
	err = os.MkdirAll(config.PathSendItems, 0755)
	if err != nil {
		log.Fatalln(err)
	}
	err = os.MkdirAll(config.PathStorages, 0755)
	if err != nil {
		log.Fatalln(err)
	}
	err = os.MkdirAll(config.PathMedia, 0755)
	if err != nil {
		log.Fatalln(err)
	}

	engine := html.NewFileSystem(http.FS(EmbedIndex), ".html")
	engine.AddFunc("isEnableBasicAuth", func(token any) bool {
		return token != nil
	})
	app := fiber.New(fiber.Config{
		Views:     engine,
		BodyLimit: int(config.WhatsappSettingMaxVideoSize),
	})

	app.Static("/statics", "./statics")
	app.Use("/components", filesystem.New(filesystem.Config{
		Root:       http.FS(EmbedViews),
		PathPrefix: "views/components",
		Browse:     true,
	}))
	app.Use("/assets", filesystem.New(filesystem.Config{
		Root:       http.FS(EmbedViews),
		PathPrefix: "views/assets",
		Browse:     true,
	}))

	app.Use(middleware.Recovery())
	app.Use(middleware.BasicAuth())
	if config.AppDebug {
		app.Use(logger.New())
	}
	app.Use(cors.New(cors.Config{
		AllowOrigins: "*",
		AllowHeaders: "Origin, Content-Type, Accept",
	}))

	if len(config.AppBasicAuthCredential) > 0 {
		account := make(map[string]string)
		for _, basicAuth := range config.AppBasicAuthCredential {
			ba := strings.Split(basicAuth, ":")
			if len(ba) != 2 {
				log.Fatalln("Basic auth is not valid, please this following format <user>:<secret>")
			}
			account[ba[0]] = ba[1]
		}

		app.Use(basicauth.New(basicauth.Config{
			Users: account,
		}))
	}

	// Endpoint para enviar mensagens com citação
	app.Post("/send/message", func(c *fiber.Ctx) error {
		var request struct {
			Phone          string `json:"phone"`
			Message        string `json:"message"`
			ReplyMessageID string `json:"reply_message_id"`
		}
		if err := c.BodyParser(&request); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Invalid request body"})
		}

		if request.Phone == "" || request.Message == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Phone and message are required"})
		}

		waCli := whatsapp.GetWaCli()
		if waCli == nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "WhatsApp client not initialized"})
		}

		if !waCli.IsConnected() || !waCli.IsLoggedIn() {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "WhatsApp client not connected or logged in"})
		}

		jid, err := whatsapp.ParseJID(request.Phone)
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": fmt.Sprintf("Invalid Phone: %v", err)})
		}

		msg := &waProto.Message{
			ExtendedTextMessage: &waProto.ExtendedTextMessage{
				Text: proto.String(request.Message),
			},
		}

		// Adicionar citação se reply_message_id for fornecido
		if request.ReplyMessageID != "" {
			msg.ExtendedTextMessage.ContextInfo = &waProto.ContextInfo{
				StanzaID:      proto.String(request.ReplyMessageID),
				Participant:   proto.String(request.Phone),
				QuotedMessage: &waProto.Message{Conversation: proto.String("")},
			}
		}

		_, err = waCli.SendMessage(context.Background(), jid, msg)
		if err != nil {
			logrus.Errorf("Failed to send message to %s: %v", jid.String(), err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": fmt.Sprintf("Failed to send message: %v", err)})
		}
		logrus.Infof("Message sent successfully to %s", jid.String())

		return c.JSON(fiber.Map{"status": "Message sent"})
	})

	app.Post("/send-presence", func(c *fiber.Ctx) error {
		var request struct {
			Phone    string `json:"Phone"`
			Presence string `json:"presence"`
			Duration int64  `json:"duration"`
		}
		if err := c.BodyParser(&request); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Invalid request body"})
		}

		if request.Phone == "" || request.Presence == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Phone and presence are required"})
		}

		waCli := whatsapp.GetWaCli()
		if waCli == nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "WhatsApp client not initialized"})
		}

		jid, err := whatsapp.ParseJID(request.Phone)
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": fmt.Sprintf("Invalid Phone: %v", err)})
		}

		var presence types.ChatPresence
		var media types.ChatPresenceMedia
		switch request.Presence {
		case "typing":
			presence = types.ChatPresenceComposing
			media = types.ChatPresenceMediaText
		case "recording":
			presence = types.ChatPresenceComposing
			media = types.ChatPresenceMediaAudio
		default:
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Invalid presence type, must be 'typing' or 'recording'"})
		}

		err = waCli.SendChatPresence(jid, presence, media)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": fmt.Sprintf("Failed to send presence: %v", err)})
		}

		if request.Duration > 0 {
			go func() {
				time.Sleep(time.Duration(request.Duration) * time.Second)
				if err := waCli.SendChatPresence(jid, types.ChatPresencePaused, types.ChatPresenceMediaText); err != nil {
					logrus.Errorf("Failed to send paused presence: %v", err)
				}
			}()
		}

		return c.JSON(fiber.Map{"status": fmt.Sprintf("Presence %s sent to %s", request.Presence, request.Phone)})
	})

	app.Post("/call-ended", func(c *fiber.Ctx) error {
		var request struct {
			CallID string `json:"call_id"`
			Phone  string `json:"Phone"`
		}
		if err := c.BodyParser(&request); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Invalid request body"})
		}

		if request.CallID == "" || request.Phone == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "call_id and Phone are required"})
		}

		waCli := whatsapp.GetWaCli()
		if waCli == nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "WhatsApp client not initialized"})
		}

		jid, err := whatsapp.ParseJID(request.Phone)
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": fmt.Sprintf("Invalid Phone: %v", err)})
		}

		cacheKey := request.CallID + ":" + request.Phone
		if _, exists := callWebhookCache.LoadOrStore(cacheKey, time.Now()); exists {
			logrus.Infof("Webhook para call_id %s e Phone %s já enviado, ignorando", request.CallID, request.Phone)
			return c.JSON(fiber.Map{
				"status":  "call rejected (already processed)",
				"call_id": request.CallID,
				"Phone":   request.Phone,
			})
		}

		go func() {
			time.Sleep(cacheTTL)
			callWebhookCache.Delete(cacheKey)
		}()

		err = waCli.RejectCall(jid, request.CallID)
		if err != nil {
			callWebhookCache.Delete(cacheKey)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": fmt.Sprintf("Failed to reject call: %v", err)})
		}

		if len(config.WhatsappWebhook) > 0 {
			go func() {
				payload := map[string]interface{}{
					"SenderNumber": request.Phone,
					"Call_Id":      request.CallID,
					"Type":         "call_received",
					"Status_Call":  "rejected",
					"timestamp":    time.Now().Format(time.RFC3339),
					"IsGroup":      strings.Contains(request.Phone, "@g.us"),
				}
				for _, url := range config.WhatsappWebhook {
					if err := whatsapp.SubmitWebhook(payload, url); err != nil {
						logrus.Errorf("Failed to send call rejected webhook: %v", err)
					}
				}
			}()
		}

		return c.JSON(fiber.Map{
			"status":  "call rejected",
			"call_id": request.CallID,
			"Phone":   request.Phone,
		})
	})

	app.Post("/chat/send/audio", func(c *fiber.Ctx) error {
		var request struct {
			Phone string `json:"Phone"`
			Media string `json:"media"`
		}
		if err := c.BodyParser(&request); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Invalid request body"})
		}

		if request.Phone == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Phone is required"})
		}

		waCli := whatsapp.GetWaCli()
		if waCli == nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "WhatsApp client not initialized"})
		}

		if !waCli.IsConnected() || !waCli.IsLoggedIn() {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "WhatsApp client not connected or logged in"})
		}

		jid, err := whatsapp.ParseJID(request.Phone)
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": fmt.Sprintf("Invalid Phone: %v", err)})
		}

		var audioData []byte
		var mimeType string

		if strings.HasPrefix(request.Media, "data:audio/") || strings.Contains(request.Media, ",") {
			parts := strings.SplitN(request.Media, ",", 2)
			if len(parts) != 2 {
				return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Invalid Base64 format"})
			}
			mimeType = strings.TrimPrefix(strings.Split(parts[0], ";")[0], "data:")
			audioData, err = base64.StdEncoding.DecodeString(parts[1])
			if err != nil {
				return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": fmt.Sprintf("Failed to decode Base64: %v", err)})
			}
		} else {
			if _, err := os.Stat(request.Media); os.IsNotExist(err) {
				return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": fmt.Sprintf("File not found: %s", request.Media)})
			}
			audioData, err = os.ReadFile(request.Media)
			if err != nil {
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": fmt.Sprintf("Failed to read file: %v", err)})
			}
			mimeType = determineMimeType(request.Media)
			if mimeType == "" {
				mimeType = http.DetectContentType(audioData)
				logrus.Warnf("MIME type not detected by extension for file %s, auto-detected as %s", request.Media, mimeType)
			}
		}

		switch mimeType {
		case "audio/opus", "audio/ogg":
			mimeType = "audio/ogg"
		case "audio/mpeg", "audio/mp3":
			mimeType = "audio/mpeg"
		case "audio/wav":
			mimeType = "audio/wav"
		case "audio/aac":
			mimeType = "audio/aac"
		default:
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": fmt.Sprintf("Unsupported audio format: %s", mimeType)})
		}
		logrus.Infof("Detected MIME type for media: %s", mimeType)

		tempPath := filepath.Join(config.PathMedia, fmt.Sprintf("temp_%s", filepath.Base(request.Media)))
		if err := os.WriteFile(tempPath, audioData, 0644); err != nil {
			logrus.Errorf("Failed to save temp file: %v", err)
		} else {
			logrus.Infof("Temporary file saved at %s for debugging", tempPath)
		}

		err = whatsapp.SendAudioMessage(context.Background(), jid, audioData, mimeType)
		if err != nil {
			logrus.Errorf("Failed to send audio message to %s: %v", jid.String(), err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": fmt.Sprintf("Failed to send audio message: %v", err)})
		}
		logrus.Infof("Audio message sent successfully to %s", jid.String())

		return c.JSON(fiber.Map{"status": "Audio sent"})
	})

	app.Post("/chat/send/document", func(c *fiber.Ctx) error {
		var request struct {
			Phone        string `json:"Phone"`
			FileName     string `json:"FileName"`
			Caption      string `json:"Caption"`
			DocumentPath string `json:"DocumentPath"`
			IsForwarded  bool   `json:"is_forwarded"`
		}
		if err := c.BodyParser(&request); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Invalid request body"})
		}

		if request.Phone == "" || request.DocumentPath == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Phone and DocumentPath are required"})
		}

		waCli := whatsapp.GetWaCli()
		if waCli == nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "WhatsApp client not initialized"})
		}

		if !waCli.IsConnected() || !waCli.IsLoggedIn() {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "WhatsApp client not connected or logged in"})
		}

		jid, err := whatsapp.ParseJID(request.Phone)
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": fmt.Sprintf("Invalid Phone: %v", err)})
		}

		if _, err := os.Stat(request.DocumentPath); os.IsNotExist(err) {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": fmt.Sprintf("File not found: %s", request.DocumentPath)})
		}
		documentData, err := os.ReadFile(request.DocumentPath)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": fmt.Sprintf("Failed to read file: %v", err)})
		}

		if int64(len(documentData)) > config.WhatsappSettingMaxFileSize {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": fmt.Sprintf("Document size exceeds the maximum limit of %d bytes", config.WhatsappSettingMaxFileSize)})
		}

		mimeType := determineMimeType(request.DocumentPath)
		if mimeType == "" {
			mimeType = http.DetectContentType(documentData)
			logrus.Warnf("MIME type not detected by extension for file %s, auto-detected as %s", request.DocumentPath, mimeType)
		}

		tempPath := filepath.Join(config.PathMedia, fmt.Sprintf("temp_%s", request.FileName))
		if err := os.WriteFile(tempPath, documentData, 0644); err != nil {
			logrus.Errorf("Failed to save temp file: %v", err)
		} else {
			logrus.Infof("Temporary file saved at %s for debugging", tempPath)
		}

		err = whatsapp.SendDocumentMessage(context.Background(), jid, documentData, mimeType, request.FileName, request.Caption, request.IsForwarded)
		if err != nil {
			logrus.Errorf("Failed to send document message to %s: %v", jid.String(), err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": fmt.Sprintf("Failed to send document message: %v", err)})
		}
		logrus.Infof("Document message sent successfully to %s", jid.String())

		return c.JSON(fiber.Map{"status": "Document sent"})
	})

	app.Post("/chat/send/video", func(c *fiber.Ctx) error {
		var request struct {
			Phone       string `json:"Phone"`
			Caption     string `json:"Caption"`
			VideoPath   string `json:"VideoPath"`
			ViewOnce    bool   `json:"view_once"`
			IsForwarded bool   `json:"is_forwarded"`
		}
		if err := c.BodyParser(&request); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Invalid request body"})
		}

		if request.Phone == "" || request.VideoPath == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Phone and VideoPath are required"})
		}

		waCli := whatsapp.GetWaCli()
		if waCli == nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "WhatsApp client not initialized"})
		}

		if !waCli.IsConnected() || !waCli.IsLoggedIn() {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "WhatsApp client not connected or logged in"})
		}

		jid, err := whatsapp.ParseJID(request.Phone)
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": fmt.Sprintf("Invalid Phone: %v", err)})
		}

		if _, err := os.Stat(request.VideoPath); os.IsNotExist(err) {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": fmt.Sprintf("File not found: %s", request.VideoPath)})
		}
		videoData, err := os.ReadFile(request.VideoPath)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": fmt.Sprintf("Failed to read file: %v", err)})
		}

		if int64(len(videoData)) > config.WhatsappSettingMaxVideoSize {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": fmt.Sprintf("Video size exceeds the maximum limit of %d bytes", config.WhatsappSettingMaxVideoSize)})
		}

		mimeType := determineMimeType(request.VideoPath)
		if mimeType == "" {
			mimeType = http.DetectContentType(videoData)
			logrus.Warnf("MIME type not detected by extension for file %s, auto-detected as %s", request.VideoPath, mimeType)
		}

		tempPath := filepath.Join(config.PathMedia, fmt.Sprintf("temp_%s", filepath.Base(request.VideoPath)))
		if err := os.WriteFile(tempPath, videoData, 0644); err != nil {
			logrus.Errorf("Failed to save temp file: %v", err)
		} else {
			logrus.Infof("Temporary file saved at %s for debugging", tempPath)
		}

		err = whatsapp.SendVideoMessage(context.Background(), jid, videoData, mimeType, filepath.Base(request.VideoPath), request.Caption, request.ViewOnce, request.IsForwarded)
		if err != nil {
			logrus.Errorf("Failed to send video message to %s: %v", jid.String(), err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": fmt.Sprintf("Failed to send video message: %v", err)})
		}
		logrus.Infof("Video message sent successfully to %s", jid.String())

		return c.JSON(fiber.Map{"status": "Video sent"})
	})

	app.Post("/chat/send/image", func(c *fiber.Ctx) error {
		var request struct {
			Phone       string `json:"Phone"`
			Caption     string `json:"Caption"`
			ImagePath   string `json:"ImagePath"`
			ViewOnce    bool   `json:"view_once"`
			IsForwarded bool   `json:"is_forwarded"`
		}
		if err := c.BodyParser(&request); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Invalid request body"})
		}

		if request.Phone == "" || request.ImagePath == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Phone and ImagePath are required"})
		}

		waCli := whatsapp.GetWaCli()
		if waCli == nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "WhatsApp client not initialized"})
		}

		if !waCli.IsConnected() || !waCli.IsLoggedIn() {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "WhatsApp client not connected or logged in"})
		}

		jid, err := whatsapp.ParseJID(request.Phone)
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": fmt.Sprintf("Invalid Phone: %v", err)})
		}

		if _, err := os.Stat(request.ImagePath); os.IsNotExist(err) {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": fmt.Sprintf("File not found: %s", request.ImagePath)})
		}
		imageData, err := os.ReadFile(request.ImagePath)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": fmt.Sprintf("Failed to read file: %v", err)})
		}

		if int64(len(imageData)) > config.WhatsappSettingMaxFileSize {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": fmt.Sprintf("Image size exceeds the maximum limit of %d bytes", config.WhatsappSettingMaxFileSize)})
		}

		mimeType := determineMimeType(request.ImagePath)
		if mimeType == "" {
			mimeType = http.DetectContentType(imageData)
			logrus.Warnf("MIME type not detected by extension for file %s, auto-detected as %s", request.ImagePath, mimeType)
		}

		tempPath := filepath.Join(config.PathMedia, fmt.Sprintf("temp_%s", filepath.Base(request.ImagePath)))
		if err := os.WriteFile(tempPath, imageData, 0644); err != nil {
			logrus.Errorf("Failed to save temp file: %v", err)
		} else {
			logrus.Infof("Temporary file saved at %s for debugging", tempPath)
		}

		err = whatsapp.SendImageMessage(context.Background(), jid, imageData, mimeType, filepath.Base(request.ImagePath), request.Caption, request.ViewOnce, request.IsForwarded)
		if err != nil {
			logrus.Errorf("Failed to send image message to %s: %v", jid.String(), err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": fmt.Sprintf("Failed to send image message: %v", err)})
		}
		logrus.Infof("Image message sent successfully to %s", jid.String())

		return c.JSON(fiber.Map{"status": "Image sent"})
	})

	app.Post("/chat/send/location", func(c *fiber.Ctx) error {
		var request struct {
			Phone     string  `json:"Phone"`
			Latitude  float64 `json:"latitude"`
			Longitude float64 `json:"longitude"`
		}
		if err := c.BodyParser(&request); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Invalid request body"})
		}

		if request.Phone == "" || request.Latitude == 0 || request.Longitude == 0 {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Phone, latitude, and longitude are required"})
		}

		waCli := whatsapp.GetWaCli()
		if waCli == nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "WhatsApp client not initialized"})
		}

		if !waCli.IsConnected() || !waCli.IsLoggedIn() {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "WhatsApp client not connected or logged in"})
		}

		jid, err := whatsapp.ParseJID(request.Phone)
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": fmt.Sprintf("Invalid Phone: %v", err)})
		}

		err = whatsapp.SendLocationMessage(context.Background(), jid, request.Latitude, request.Longitude)
		if err != nil {
			logrus.Errorf("Failed to send location message to %s: %v", jid.String(), err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": fmt.Sprintf("Failed to send location message: %v", err)})
		}
		logrus.Infof("Location message sent successfully to %s", jid.String())

		return c.JSON(fiber.Map{"status": "Location sent"})
	})

	app.Post("/chat/delete-message", func(c *fiber.Ctx) error {
		var request struct {
			Phone     string `json:"Phone"`
			MessageID string `json:"message_id"`
		}
		if err := c.BodyParser(&request); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Invalid request body"})
		}

		if request.Phone == "" || request.MessageID == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Phone and message_id are required"})
		}

		waCli := whatsapp.GetWaCli()
		if waCli == nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "WhatsApp client not initialized"})
		}

		if !waCli.IsConnected() || !waCli.IsLoggedIn() {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "WhatsApp client not connected or logged in"})
		}

		jid, err := whatsapp.ParseJID(request.Phone)
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": fmt.Sprintf("Invalid Phone: %v", err)})
		}

		messageID := types.MessageID(request.MessageID)
		_, err = waCli.RevokeMessage(jid, messageID)
		if err != nil {
			logrus.Errorf("Failed to revoke message %s in chat %s: %v", messageID, jid.String(), err)
			if strings.Contains(err.Error(), "too old") || strings.Contains(err.Error(), "not allowed") {
				return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Message deletion not allowed: likely too old or not sent by you"})
			}
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": fmt.Sprintf("Failed to revoke message: %v", err)})
		}
		logrus.Infof("Message %s revoked successfully in chat %s", messageID, jid.String())

		return c.JSON(fiber.Map{"status": fmt.Sprintf("Message %s deleted", messageID)})
	})

	app.Post("/chat/mark-read", func(c *fiber.Ctx) error {
		var request struct {
			Phone     string `json:"phone"`
			MessageID string `json:"message_id"`
			Sender    string `json:"sender,omitempty"`
			Played    string `json:"played"` // Alterado de bool para string
		}
		if err := c.BodyParser(&request); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Invalid request body"})
		}

		if request.Phone == "" || request.MessageID == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Phone and message_id are required"})
		}

		waCli := whatsapp.GetWaCli()
		if waCli == nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "WhatsApp client not initialized"})
		}

		if !waCli.IsConnected() || !waCli.IsLoggedIn() {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "WhatsApp client not connected or logged in"})
		}

		chatJID, err := whatsapp.ParseJID(request.Phone)
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": fmt.Sprintf("Invalid Phone: %v", err)})
		}

		var senderJID types.JID
		if request.Sender != "" {
			senderJID, err = whatsapp.ParseJID(request.Sender)
			if err != nil {
				return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": fmt.Sprintf("Invalid sender JID: %v", err)})
			}
		} else if strings.Contains(chatJID.String(), "@g.us") {
			logrus.Warnf("Sender not provided for group chat %s, marking read may fail", chatJID.String())
		}

		messageID := types.MessageID(request.MessageID)
		timestamp := time.Now()

		var receiptTypeExtra []types.ReceiptType
		switch strings.ToLower(request.Played) {
		case "audio":
			receiptTypeExtra = append(receiptTypeExtra, types.ReceiptTypePlayed)
		case "text":
			receiptTypeExtra = append(receiptTypeExtra, types.ReceiptTypeRead)
		default:
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Invalid 'played' value. Use 'audio' or 'text'"})
		}

		logrus.Debugf("Marking message %s as read in chat %s with sender %s, played: %s", messageID, chatJID.String(), senderJID.String(), request.Played)
		err = waCli.MarkRead([]types.MessageID{messageID}, timestamp, chatJID, senderJID, receiptTypeExtra...)
		if err != nil {
			logrus.Errorf("Failed to mark message %s as read in chat %s: %v", messageID, chatJID.String(), err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": fmt.Sprintf("Failed to mark message as read: %v", err)})
		}
		logrus.Infof("Message %s marked as read in chat %s", messageID, chatJID.String())

		return c.JSON(fiber.Map{"status": fmt.Sprintf("Message %s marked as %s", messageID, request.Played)})
	})

	rest.InitRestApp(app, appUsecase)
	rest.InitRestSend(app, sendUsecase)
	rest.InitRestUser(app, userUsecase)
	rest.InitRestMessage(app, messageUsecase)
	rest.InitRestGroup(app, groupUsecase)
	rest.InitRestNewsletter(app, newsletterUsecase)

	app.Get("/", func(c *fiber.Ctx) error {
		return c.Render("views/index", fiber.Map{
			"AppHost":        fmt.Sprintf("%s://%s", c.Protocol(), c.Hostname()),
			"AppVersion":     config.AppVersion,
			"BasicAuthToken": c.UserContext().Value(middleware.AuthorizationValue("BASIC_AUTH")),
			"MaxFileSize":    humanize.Bytes(uint64(config.WhatsappSettingMaxFileSize)),
			"MaxVideoSize":   humanize.Bytes(uint64(config.WhatsappSettingMaxVideoSize)),
		})
	})

	websocket.RegisterRoutes(app, appUsecase)
	go websocket.RunHub()

	go helpers.SetAutoConnectAfterBooting(appUsecase)
	go helpers.SetAutoReconnectChecking(whatsapp.GetWaCli())
	if config.WhatsappChatStorage {
		go helpers.StartAutoFlushChatStorage()
	}

	if err := app.Listen(":" + config.AppPort); err != nil {
		log.Fatalln("Failed to start: ", err.Error())
	}
}

func determineMimeType(filename string) string {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(filename), "."))
	switch ext {
	case "mp3":
		return "audio/mpeg"
	case "ogg":
		return "audio/ogg"
	case "wav":
		return "audio/wav"
	case "aac":
		return "audio/aac"
	case "opus":
		return "audio/opus"
	case "mp4":
		return "video/mp4"
	case "jpg", "jpeg":
		return "image/jpeg"
	case "png":
		return "image/png"
	case "gif":
		return "image/gif"
	case "pdf":
		return "application/pdf"
	case "doc", "docx":
		return "application/msword"
	case "xls", "xlsx":
		return "application/vnd.ms-excel"
	default:
		return ""
	}
}
