package compat

import (
	"context"
	"encoding/json"

	"github.com/spirilis/generic-go-mcp/mcp"
	"github.com/spirilis/generic-go-mcp/transport"
)

// legacyListenFilter is the subscriptions/listen filter this overlay always asks the inner
// server for on behalf of a legacy stream: both list_changed notification types.
// notifications/resources/updated has no legacy equivalent to opt into — the legacy
// protocol has no per-resource subscription concept this overlay implements
// (resources/subscribe and resources/unsubscribe are both -32601 here) — so it is never
// requested.
var legacyListenFilter = map[string]interface{}{
	"toolsListChanged":     true,
	"resourcesListChanged": true,
}

// legacyNotificationWriter adapts a subscriptions/listen stream from the inner modern
// handler into the shape a legacy client expects: it drops the
// notifications/subscriptions/acknowledged ack (a modern-only concept a legacy client
// never asked for and wouldn't recognize), and strips the
// io.modelcontextprotocol/subscriptionId _meta tag from every notification it relays — that
// tag exists to demultiplex several concurrent listen requests sharing one stdio/UNIX
// connection, a distinction the legacy protocol, and this overlay's one-stream-per-session
// model, has no use for.
type legacyNotificationWriter struct {
	out transport.ResponseWriter
}

func (w *legacyNotificationWriter) WriteNotification(method string, params interface{}) error {
	if method == "notifications/subscriptions/acknowledged" {
		return nil
	}
	return w.out.WriteNotification(method, stripSubscriptionMeta(params))
}

func (w *legacyNotificationWriter) WriteMessage(data []byte) error {
	// mcp.Server's subscriptions/listen handler never calls this on the "abrupt
	// disconnect" path a context cancellation always takes (see the simplification
	// documented on mcp/subscriptions.go's handleSubscriptionsListen) — implement it
	// rather than silently swallow a final response if that ever changes.
	return w.out.WriteMessage(data)
}

// stripSubscriptionMeta removes the io.modelcontextprotocol/subscriptionId tag the inner
// server's Broker attaches to every relayed notification (mcp/subscriptions.go's
// withSubscriptionMeta), dropping _meta entirely if that leaves it empty. params is always
// a map[string]interface{} in practice — that's what withSubscriptionMeta builds — but any
// other shape is passed through unchanged rather than guessed at.
func stripSubscriptionMeta(params interface{}) interface{} {
	m, ok := params.(map[string]interface{})
	if !ok {
		return params
	}
	metaRaw, ok := m["_meta"]
	if !ok {
		return params
	}
	meta, ok := metaRaw.(map[string]interface{})
	if !ok {
		return params
	}

	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		out[k] = v
	}
	strippedMeta := make(map[string]interface{}, len(meta))
	for k, v := range meta {
		if k == "io.modelcontextprotocol/subscriptionId" {
			continue
		}
		strippedMeta[k] = v
	}
	if len(strippedMeta) == 0 {
		delete(out, "_meta")
	} else {
		out["_meta"] = strippedMeta
	}
	return out
}

// listenParams marshals a subscriptions/listen request's params, opting into
// legacyListenFilter.
func listenParams() json.RawMessage {
	out, _ := json.Marshal(struct {
		Notifications map[string]interface{} `json:"notifications"`
	}{Notifications: legacyListenFilter})
	return out
}

// StreamNotifications implements transport.LegacySessions. It synthesizes a
// subscriptions/listen request against the inner modern handler on behalf of the legacy
// session named id, relaying tools/resources list_changed notifications to w — in the
// legacy shape, via legacyNotificationWriter — until ctx is done. It blocks for the
// stream's entire lifetime, mirroring how the inner subscriptions/listen handler itself
// blocks; the caller (the Streamable HTTP binding's GET handler, or a stdio listener
// goroutine started after a successful handshake) is responsible for deriving ctx from
// both the underlying connection and this session's own Done channel, so that either one
// ending the stream is honored.
func (s *sessionStore) StreamNotifications(ctx context.Context, id string, w transport.ResponseWriter) error {
	s.touch(id)

	caps := &mcp.ClientCapabilities{}
	body := buildModernRequest(syntheticRequestID, "subscriptions/listen", listenParams(), caps, nil)
	s.inner.HandleMessage(ctx, body, &legacyNotificationWriter{out: w})
	return nil
}

var _ transport.LegacySessions = (*sessionStore)(nil)
