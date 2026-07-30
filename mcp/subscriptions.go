package mcp

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/spirilis/generic-go-mcp/transport"
)

// NotificationFilter is the set of change-notification types a subscriptions/listen
// caller opts into. All fields are optional; omitting one is equivalent to not
// subscribing to that notification type.
type NotificationFilter struct {
	ToolsListChanged      bool     `json:"toolsListChanged,omitempty"`
	PromptsListChanged    bool     `json:"promptsListChanged,omitempty"`
	ResourcesListChanged  bool     `json:"resourcesListChanged,omitempty"`
	ResourceSubscriptions []string `json:"resourceSubscriptions,omitempty"`
}

type subscriptionsListenParams struct {
	Notifications NotificationFilter `json:"notifications"`
}

type pendingNotification struct {
	method string
	params map[string]interface{}
}

type subscription struct {
	filter NotificationFilter
	ch     chan pendingNotification
}

// Broker fans registry-mutation notifications out to every live subscriptions/listen
// stream that opted into them. A Server owns exactly one Broker.
type Broker struct {
	mu   sync.Mutex
	subs map[*subscription]struct{}
}

func newBroker() *Broker {
	return &Broker{subs: make(map[*subscription]struct{})}
}

func (b *Broker) subscribe(filter NotificationFilter) *subscription {
	sub := &subscription{filter: filter, ch: make(chan pendingNotification, 16)}
	b.mu.Lock()
	b.subs[sub] = struct{}{}
	b.mu.Unlock()
	return sub
}

func (b *Broker) unsubscribe(sub *subscription) {
	b.mu.Lock()
	delete(b.subs, sub)
	b.mu.Unlock()
}

func (b *Broker) broadcast(want func(NotificationFilter) bool, method string, params map[string]interface{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for sub := range b.subs {
		if !want(sub.filter) {
			continue
		}
		select {
		case sub.ch <- pendingNotification{method: method, params: params}:
		default:
			// Slow subscriber: drop rather than block the registry mutation that
			// triggered this notification.
		}
	}
}

func (b *Broker) notifyToolsListChanged() {
	b.broadcast(func(f NotificationFilter) bool { return f.ToolsListChanged }, "notifications/tools/list_changed", nil)
}

func (b *Broker) notifyResourcesListChanged() {
	b.broadcast(func(f NotificationFilter) bool { return f.ResourcesListChanged }, "notifications/resources/list_changed", nil)
}

// metaWithSubscriptionID builds the _meta object every message on a subscriptions/listen
// stream must carry, tagging it with the JSON-RPC id of the originating listen request so
// a client juggling several concurrent subscriptions on one shared channel (stdio/UNIX)
// can demultiplex them.
func metaWithSubscriptionID(id json.RawMessage) map[string]interface{} {
	return map[string]interface{}{metaKeySubscriptionID: id}
}

func withSubscriptionMeta(params map[string]interface{}, id json.RawMessage) map[string]interface{} {
	out := map[string]interface{}{"_meta": metaWithSubscriptionID(id)}
	for k, v := range params {
		out[k] = v
	}
	return out
}

// handleSubscriptionsListen implements subscriptions/listen: it acknowledges the
// subscription, then blocks relaying matching notifications until ctx is cancelled
// (the client closed its stream on HTTP, or sent notifications/cancelled on stdio/UNIX).
//
// This is a long-lived request handled outside the uniform Result-returning dispatch in
// HandleMessage, since it needs to write directly through the ResponseWriter rather than
// return a single result.
//
// Simplification: on ctx cancellation this returns without writing a final JSON-RPC
// response, treating every termination as the "abrupt disconnect" case. The spec's
// "graceful closure" (an empty result response before closing, for server-initiated
// teardown) is a SHOULD, not a MUST, and is not implemented in this round.
func (s *Server) handleSubscriptionsListen(ctx context.Context, id json.RawMessage, params json.RawMessage, w transport.ResponseWriter) {
	var p subscriptionsListenParams
	if len(params) > 0 {
		_ = json.Unmarshal(params, &p)
	}

	sub := s.broker.subscribe(p.Notifications)
	defer s.broker.unsubscribe(sub)

	ackParams := withSubscriptionMeta(map[string]interface{}{"notifications": p.Notifications}, id)
	if err := w.WriteNotification("notifications/subscriptions/acknowledged", ackParams); err != nil {
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case n := <-sub.ch:
			_ = w.WriteNotification(n.method, withSubscriptionMeta(n.params, id))
		}
	}
}
