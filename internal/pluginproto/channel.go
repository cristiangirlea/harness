package pluginproto

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
)

const (
	KindCapability = "capability"
	KindChannel    = "channel"

	TypeInbound        = "inbound"
	TypeAck            = "ack"
	TypeDeliver        = "deliver"
	TypeDelivered      = "delivered"
	TypeDeliveryFailed = "delivery_failed"
	TypeStatus         = "status"

	ChatPrivate = "private"
	ChatGroup   = "group"
	ChatChannel = "channel"

	StateConnected    = "connected"
	StateReconnecting = "reconnecting"
	StateUnauthorised = "unauthorised"
	StateStopped      = "stopped"

	MaxAttachments = 16
	MaxButtons     = 8
)

var (
	timestampShape       = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}[Tt][0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?([Zz]|[+-]([01][0-9]|2[0-3]):[0-5][0-9])$`)
	channelName          = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)
	buttonID             = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	ErrChannelRequiresV2 = errors.New("ready.kind: channel plugin requires protocol v2")
)

type Channel struct {
	Name  string `json:"name"`
	Label string `json:"label,omitempty"`
}

type Sender struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

type Chat struct {
	ID   string  `json:"id"`
	Type *string `json:"type,omitempty"`
}

type Action struct {
	DeliveryID string `json:"delivery_id"`
	ButtonID   string `json:"button_id"`
}

type Attachment struct {
	Name string `json:"name"`
	MIME string `json:"mime"`
	Size int64  `json:"size" required:"true"`
	Ref  string `json:"ref"`
}

type Inbound struct {
	V           int          `json:"v"`
	Seq         int64        `json:"seq"`
	Type        string       `json:"type"`
	ID          string       `json:"id"`
	Time        string       `json:"time"`
	Sender      Sender       `json:"sender"`
	Chat        Chat         `json:"chat"`
	Text        string       `json:"text" required:"true"`
	ReplyTo     *string      `json:"reply_to,omitempty"`
	Action      *Action      `json:"action,omitempty"`
	Attachments []Attachment `json:"attachments,omitempty"`
}

type Ack struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

type Button struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type Deliver struct {
	Type    string   `json:"type"`
	ID      string   `json:"id"`
	ChatID  string   `json:"chat_id"`
	Text    string   `json:"text" required:"true"`
	ReplyTo *string  `json:"reply_to,omitempty"`
	Buttons []Button `json:"buttons,omitempty"`
}

type Delivered struct {
	V         int    `json:"v"`
	Seq       int64  `json:"seq"`
	Type      string `json:"type"`
	ID        string `json:"id"`
	MessageID string `json:"message_id"`
}

type DeliveryFailed struct {
	V            int     `json:"v"`
	Seq          int64   `json:"seq"`
	Type         string  `json:"type"`
	ID           string  `json:"id"`
	Error        Problem `json:"error"`
	RetryAfterMS *int64  `json:"retry_after_ms,omitempty"`
}

type Status struct {
	V     int    `json:"v"`
	Seq   int64  `json:"seq"`
	Type  string `json:"type"`
	State string `json:"state"`
	Text  string `json:"text,omitempty"`
}

func validateControls(path, value string, multiline bool) error {
	for _, r := range value {
		if multiline && (r == '\n' || r == '\r' || r == '\t') {
			continue
		}
		if unicode.IsControl(r) {
			return fmt.Errorf("%s: contains a control character", path)
		}
	}
	return nil
}

func shortString(path, value string, minBytes, maxBytes int) error {
	if len(value) < minBytes || len(value) > maxBytes {
		return fmt.Errorf("%s: expected %d to %d bytes", path, minBytes, maxBytes)
	}
	return validateControls(path, value, false)
}

func serviceID(path, id string) error {
	return shortString(path, id, 1, 128)
}

func validateMessageText(path, value string) error {
	if len(value) > MaxEventText {
		return fmt.Errorf("%s: exceeds %d bytes", path, MaxEventText)
	}
	return validateControls(path, value, true)
}

func validateReplyTo(path string, id *string) error {
	if id == nil {
		return nil
	}
	return serviceID(path+".reply_to", *id)
}

func validateDeliveryID(path, id string) error {
	if !callID.MatchString(id) {
		return fmt.Errorf("%s: invalid delivery ID", path)
	}
	return nil
}

func validateInboundTime(value string) error {
	// Go accepts one-digit hours, comma fractions and overflowing zone offsets.
	if !timestampShape.MatchString(value) {
		return fmt.Errorf("inbound.time: expected RFC 3339")
	}
	value = strings.ToUpper(value)
	leap := value[17:19] == "60"
	if leap {
		value = value[:17] + "59" + value[19:]
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return fmt.Errorf("inbound.time: %w", err)
	}
	if leap {
		next := t.UTC().Add(time.Second)
		if next.Day() != 1 || next.Hour() != 0 || next.Minute() != 0 {
			return fmt.Errorf("inbound.time: leap second must end a UTC month")
		}
	}
	return nil
}

func validateInbound(f *Inbound) error {
	if err := validateEnvelope("inbound", f.V, f.Seq); err != nil {
		return err
	}
	if err := serviceID("inbound.id", f.ID); err != nil {
		return err
	}
	if err := validateInboundTime(f.Time); err != nil {
		return err
	}
	if err := serviceID("inbound.sender.id", f.Sender.ID); err != nil {
		return err
	}
	if err := shortString("inbound.sender.name", f.Sender.Name, 0, 256); err != nil {
		return err
	}
	if err := serviceID("inbound.chat.id", f.Chat.ID); err != nil {
		return err
	}
	if f.Chat.Type != nil {
		switch *f.Chat.Type {
		case ChatPrivate, ChatGroup, ChatChannel:
		default:
			return fmt.Errorf("inbound.chat.type: unknown chat type %q", *f.Chat.Type)
		}
	}
	if err := validateReplyTo("inbound", f.ReplyTo); err != nil {
		return err
	}
	if err := validateMessageText("inbound.text", f.Text); err != nil {
		return err
	}
	if f.Action == nil && f.Text == "" {
		return fmt.Errorf("inbound.text: must be non-empty without an action")
	}
	if f.Action != nil {
		if err := validateDeliveryID("inbound.action.delivery_id", f.Action.DeliveryID); err != nil {
			return err
		}
		if !buttonID.MatchString(f.Action.ButtonID) {
			return fmt.Errorf("inbound.action.button_id: invalid button ID")
		}
	}
	for i, a := range f.Attachments {
		path := fmt.Sprintf("inbound.attachments[%d]", i)
		if err := shortString(path+".name", a.Name, 1, 256); err != nil {
			return err
		}
		if err := shortString(path+".mime", a.MIME, 1, 128); err != nil {
			return err
		}
		if a.Size < 0 {
			return fmt.Errorf("%s.size: must be non-negative", path)
		}
		if err := serviceID(path+".ref", a.Ref); err != nil {
			return err
		}
	}
	return nil
}

func validateDeliver(f *Deliver) error {
	if err := validateDeliveryID("deliver.id", f.ID); err != nil {
		return err
	}
	if err := serviceID("deliver.chat_id", f.ChatID); err != nil {
		return err
	}
	if err := validateMessageText("deliver.text", f.Text); err != nil {
		return err
	}
	if err := validateReplyTo("deliver", f.ReplyTo); err != nil {
		return err
	}
	ids := make(map[string]bool, len(f.Buttons))
	for i, b := range f.Buttons {
		if !buttonID.MatchString(b.ID) {
			return fmt.Errorf("deliver.buttons[%d].id: invalid button ID", i)
		}
		if ids[b.ID] {
			return fmt.Errorf("deliver.buttons[%d].id: duplicate button ID %q", i, b.ID)
		}
		ids[b.ID] = true
		if err := shortString(fmt.Sprintf("deliver.buttons[%d].label", i), b.Label, 1, 64); err != nil {
			return err
		}
	}
	return nil
}

func validateDelivered(f *Delivered) error {
	if err := validateEnvelope("delivered", f.V, f.Seq); err != nil {
		return err
	}
	if err := validateDeliveryID("delivered.id", f.ID); err != nil {
		return err
	}
	return serviceID("delivered.message_id", f.MessageID)
}

func validateDeliveryFailed(f *DeliveryFailed) error {
	if err := validateEnvelope("delivery_failed", f.V, f.Seq); err != nil {
		return err
	}
	if err := validateDeliveryID("delivery_failed.id", f.ID); err != nil {
		return err
	}
	if f.RetryAfterMS != nil && *f.RetryAfterMS < 0 {
		return fmt.Errorf("delivery_failed.retry_after_ms: must be non-negative")
	}
	if err := validateProblem("delivery_failed.error", f.Error.Code, f.Error.Message, V2); err != nil {
		return err
	}
	return validateControls("delivery_failed.error.message", f.Error.Message, false)
}

func validateStatus(f *Status) error {
	if err := validateEnvelope("status", f.V, f.Seq); err != nil {
		return err
	}
	switch f.State {
	case StateConnected, StateReconnecting, StateUnauthorised, StateStopped:
	default:
		return fmt.Errorf("status.state: unknown state %q", f.State)
	}
	return shortString("status.text", f.Text, 0, 1024)
}
