package paddle

import (
	"context"
	"errors"
	"github.com/data-insights-ai/rho-paddle/internal/paddlewire"
	"net/http"
	"strings"

	billing "github.com/data-insights-ai/rho-billing"
)

type NotificationSetting struct {
	Reference   billing.Reference
	Description string
	Destination string
	Active      bool
	Events      []string
	Secret      string
}

type NotificationSettingInput struct {
	Description string
	Destination string
	Events      []string
}

func (c *Client) CreateNotificationSetting(ctx context.Context, in NotificationSettingInput) (NotificationSetting, error) {
	if c == nil || !billing.ValidID(in.Description) || len(in.Description) > 500 || !paddlewire.HTTPSURL(in.Destination) || len(in.Destination) > 2048 || len(in.Events) == 0 || len(in.Events) > 64 {
		return NotificationSetting{}, ErrInvalid
	}
	for _, event := range in.Events {
		if !notificationEvent(event) {
			return NotificationSetting{}, ErrInvalid
		}
	}
	request := struct {
		Description      string   `json:"description"`
		Type             string   `json:"type"`
		Destination      string   `json:"destination"`
		APIVersion       int      `json:"api_version"`
		TrafficSource    string   `json:"traffic_source"`
		SubscribedEvents []string `json:"subscribed_events"`
	}{in.Description, "url", in.Destination, 1, "platform", append([]string(nil), in.Events...)}
	var wire paddlewire.NotificationSetting
	if err := c.request(ctx, http.MethodPost, "/notification-settings", request, &wire); err != nil {
		return NotificationSetting{}, err
	}
	out, err := c.notificationSetting(wire)
	if err != nil {
		return NotificationSetting{}, errors.Join(ErrResponse, ErrUncertain)
	}
	// The destination exists at Paddle even when it is not what was asked for,
	// so the identity and secret are returned with the error: without them the
	// host can neither verify its deliveries nor delete it.
	if out.Destination != in.Destination || !out.Active || !coversEvents(out.Events, in.Events) {
		return out, errors.Join(ErrResponse, ErrUncertain)
	}
	return out, nil
}

// coversEvents reports whether every requested event was actually subscribed.
// A partially subscribed destination silently drops whole classes of provider
// facts, so it is not a successful create.
func coversEvents(got, want []string) bool {
	have := make(map[string]struct{}, len(got))
	for _, event := range got {
		have[event] = struct{}{}
	}
	for _, event := range want {
		if _, ok := have[event]; !ok {
			return false
		}
	}
	return true
}

// NotificationSettings lists notification destinations. It is the lookup that
// resolves an uncertain create: a destination Paddle accepted but whose
// response was lost can be found here by its destination URL and then deleted
// or adopted.
func (c *Client) NotificationSettings(ctx context.Context) ([]NotificationSetting, error) {
	if c == nil {
		return nil, ErrInvalid
	}
	var wire []paddlewire.NotificationSetting
	if err := c.request(ctx, http.MethodGet, "/notification-settings", nil, &wire); err != nil {
		return nil, err
	}
	out := make([]NotificationSetting, 0, len(wire))
	for _, item := range wire {
		setting, err := c.notificationSetting(item)
		if err != nil {
			return nil, err
		}
		out = append(out, setting)
	}
	return out, nil
}

func (c *Client) DeleteNotificationSetting(ctx context.Context, ref billing.Reference) error {
	if c == nil || ref.Scope != c.scope || !paddlewire.ID(ref.ID, "ntfset_") {
		return ErrInvalid
	}
	return c.request(ctx, http.MethodDelete, "/notification-settings/"+ref.ID, nil, nil)
}

func (c *Client) notificationSetting(w paddlewire.NotificationSetting) (NotificationSetting, error) {
	if !paddlewire.ID(w.ID, "ntfset_") || w.Type != "url" || !paddlewire.HTTPSURL(w.Destination) || !notificationSecret(w.EndpointSecretKey) {
		return NotificationSetting{}, ErrResponse
	}
	events := make([]string, 0, len(w.SubscribedEvents))
	for _, event := range w.SubscribedEvents {
		if !notificationEvent(event.Name) {
			return NotificationSetting{}, ErrResponse
		}
		events = append(events, event.Name)
	}
	if len(events) == 0 {
		return NotificationSetting{}, ErrResponse
	}
	return NotificationSetting{
		Reference:   billing.Reference{Scope: c.scope, ID: w.ID},
		Description: w.Description,
		Destination: w.Destination,
		Active:      w.Active,
		Events:      events,
		Secret:      w.EndpointSecretKey,
	}, nil
}

func notificationEvent(name string) bool {
	kind, rest, ok := strings.Cut(name, ".")
	return ok && billing.ValidID(kind) && billing.ValidID(rest) && !strings.ContainsAny(name, " \t\n")
}

func notificationSecret(secret string) bool {
	const prefix = "pdl_ntfset_"
	return strings.HasPrefix(secret, prefix) && len(secret) >= len(prefix)+26 && len(secret) <= 128 && !strings.ContainsFunc(secret, func(r rune) bool { return r < 33 || r > 126 })
}
