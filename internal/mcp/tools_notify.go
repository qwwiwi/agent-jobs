package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qwwiwi/agent-jobs/internal/brain"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// JobNotifier is how a job's server reaches notify_owner: through the daemon's proxy socket
// (brain.SocketClient), which checks the job is running and holds the rate limits.
type JobNotifier interface {
	NotifyOwner(ctx context.Context, text, urgency string) (brain.Object, error)
}

// NotifyOwnerInput is notify_owner's input.
type NotifyOwnerInput struct {
	Text    string `json:"text" jsonschema:"the message for the owner, one line, at most 300 characters (longer is cut)"`
	Urgency string `json:"urgency,omitempty" jsonschema:"normal | high; default normal"`
}

// NotificationOutput is one stored notification.
type NotificationOutput struct {
	Notification *store.Notification `json:"notification"`
}

func addNotifyOwner(s *sdk.Server, d Deps) {
	sdk.AddTool(s, &sdk.Tool{
		Name: "notify_owner",
		Description: "Ask for the owner's attention: one line is typed into the agent's live session " +
			"(\"[jobs:notify-owner] ...\"), which relays it to the owner over its own channel; agent-jobs sends nothing itself. " +
			"Text is one line, at most 300 characters; urgency normal or high. A job may send at most 3, at most one a minute; " +
			"the agent at most 20 an hour. Every notification is stored; one that could not be delivered is an error and shows in status / system_doctor.",
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in NotifyOwnerInput) (*sdk.CallToolResult, NotificationOutput, error) {
		if d.JobID != "" {
			if d.JobNotifier == nil {
				return nil, NotificationOutput{}, errors.New("notify_owner: no daemon proxy in this server")
			}
			obj, err := d.JobNotifier.NotifyOwner(ctx, in.Text, in.Urgency)
			if err != nil {
				return nil, NotificationOutput{}, err
			}
			n, err := decodeNotification(obj)
			return nil, NotificationOutput{Notification: n}, err
		}
		if d.Manager == nil {
			return nil, NotificationOutput{}, errors.New("notify_owner needs the job database or a job's daemon proxy")
		}
		n, err := d.Manager.NotifyOwner("", in.Text, in.Urgency)
		return nil, NotificationOutput{Notification: n}, err
	})
}

// decodeNotification reads the proxy's answer back into a notification.
func decodeNotification(o brain.Object) (*store.Notification, error) {
	b, err := json.Marshal(o)
	if err != nil {
		return nil, fmt.Errorf("notify_owner answer: %w", err)
	}
	var n store.Notification
	if err := json.Unmarshal(b, &n); err != nil {
		return nil, fmt.Errorf("notify_owner answer: %w", err)
	}
	return &n, nil
}
