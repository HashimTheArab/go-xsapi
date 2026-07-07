package social

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/df-mc/go-xsapi/v2/rta"
	"github.com/df-mc/go-xsapi/v2/xal/xsts"
)

func TestSubscribeWithoutRTAFails(t *testing.T) {
	client := New(http.DefaultClient, nil, xsts.UserInfo{XUID: "1"}, nil)

	err := client.Subscribe(context.Background(), NopSubscriptionHandler{})
	if !errors.Is(err, rta.ErrUnavailable) {
		t.Fatalf("Subscribe error = %v, want %v", err, rta.ErrUnavailable)
	}
}

func TestSubscriptionHandlerAllowsNonComparableHandlers(t *testing.T) {
	calls := make(chan string, 1)
	handler := nonComparableSocialHandler{
		calls: calls,
		data:  []string{"non-comparable"},
	}
	c := &Client{
		subscriptionHandlers: []SubscriptionHandler{handler},
	}
	h := &subscriptionHandler{
		Client: c,
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	h.HandleEvent(json.RawMessage(`{"NotificationType":"Added","Xuids":["1","2"]}`))

	select {
	case got := <-calls:
		if got != "Added:1,2" {
			t.Fatalf("handler call = %q, want %q", got, "Added:1,2")
		}
	case <-time.After(time.Second):
		t.Fatal("handler was not called")
	}
}

func TestSubscriptionHandlerIgnoresUserUnsubscribe(t *testing.T) {
	calls := make(chan string, 1)
	handler := nonComparableSocialHandler{
		calls: calls,
		data:  []string{"non-comparable"},
	}
	c := &Client{
		subscriptionHandlers: []SubscriptionHandler{handler},
	}
	h := &subscriptionHandler{
		Client: c,
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	h.HandleError(rta.ErrUnsubscribed)

	select {
	case got := <-calls:
		t.Fatalf("handler call = %q, want none", got)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestSubscriptionHandlerNotifiesSubscriptionLost(t *testing.T) {
	calls := make(chan string, 1)
	handler := nonComparableSocialHandler{
		calls: calls,
		data:  []string{"non-comparable"},
	}
	c := &Client{
		subscriptionHandlers: []SubscriptionHandler{handler},
	}
	h := &subscriptionHandler{
		Client: c,
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	h.HandleError(io.ErrUnexpectedEOF)

	select {
	case got := <-calls:
		if got != "lost" {
			t.Fatalf("handler call = %q, want lost", got)
		}
	case <-time.After(time.Second):
		t.Fatal("subscription lost handler was not called")
	}
}

type nonComparableSocialHandler struct {
	calls chan<- string
	data  []string
}

func (h nonComparableSocialHandler) HandleSocialNotification(typ string, xuids []string) {
	h.calls <- typ + ":" + strings.Join(xuids, ",")
}

func (h nonComparableSocialHandler) HandleIncomingFriendRequestCountChange(int) {}

func (h nonComparableSocialHandler) HandleSubscriptionLost() {
	h.calls <- "lost"
}

type comparableSocialHandler struct {
	id    string
	calls chan<- string
}

func (h comparableSocialHandler) HandleSocialNotification(string, []string) { h.calls <- h.id }
func (h comparableSocialHandler) HandleIncomingFriendRequestCountChange(int) {}
func (h comparableSocialHandler) HandleSubscriptionLost()                    {}

type countingUnsubscriber struct{ calls int }

func (u *countingUnsubscriber) Unsubscribe(context.Context, *rta.Subscription) error {
	u.calls++
	return nil
}

func TestUnsubscribeRemovesOnlyTheGivenHandler(t *testing.T) {
	calls := make(chan string, 4)
	keep := comparableSocialHandler{id: "keep", calls: calls}
	remove := comparableSocialHandler{id: "remove", calls: calls}
	unsub := &countingUnsubscriber{}
	c := &Client{
		unsubscriber:         unsub,
		subscription:         rta.NewSubscription("uri", nil),
		subscriptionHandlers: []SubscriptionHandler{keep, remove},
		log:                  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	if err := c.Unsubscribe(context.Background(), remove); err != nil {
		t.Fatalf("Unsubscribe(remove): %v", err)
	}
	if unsub.calls != 0 {
		t.Fatalf("unsubscriber called %d times while a handler remains, want 0", unsub.calls)
	}

	// Only the surviving handler receives dispatched events.
	disp := &subscriptionHandler{Client: c, log: c.log}
	disp.HandleEvent(json.RawMessage(`{"NotificationType":"Added","Xuids":["1"]}`))
	select {
	case got := <-calls:
		if got != "keep" {
			t.Fatalf("event delivered to %q, want keep", got)
		}
	case <-time.After(time.Second):
		t.Fatal("surviving handler did not receive the event")
	}
	select {
	case got := <-calls:
		t.Fatalf("removed handler still received an event: %q", got)
	case <-time.After(50 * time.Millisecond):
	}

	// Removing the last handler tears down the RTA subscription.
	if err := c.Unsubscribe(context.Background(), keep); err != nil {
		t.Fatalf("Unsubscribe(keep): %v", err)
	}
	if unsub.calls != 1 {
		t.Fatalf("unsubscriber called %d times after the last handler, want 1", unsub.calls)
	}

	// Unsubscribing an unregistered handler is a no-op.
	if err := c.Unsubscribe(context.Background(), keep); err != nil {
		t.Fatalf("Unsubscribe(keep) again: %v", err)
	}
	if unsub.calls != 1 {
		t.Fatalf("unsubscriber called %d times on a no-op, want 1", unsub.calls)
	}
}

func TestUnsubscribeRejectsNonComparableHandler(t *testing.T) {
	c := &Client{
		unsubscriber:         &countingUnsubscriber{},
		subscription:         rta.NewSubscription("uri", nil),
		subscriptionHandlers: []SubscriptionHandler{nonComparableSocialHandler{}},
		log:                  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := c.Unsubscribe(context.Background(), nonComparableSocialHandler{data: []string{"x"}}); err == nil {
		t.Fatal("Unsubscribe with a non-comparable handler should return an error, not panic")
	}
}
