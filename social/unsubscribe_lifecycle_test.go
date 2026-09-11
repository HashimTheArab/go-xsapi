package social

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/df-mc/go-xsapi/v2/rta"
	"github.com/df-mc/go-xsapi/v2/xal/xsts"
)

func TestUnsubscribeRetriesFailedTeardownAndAllowsReuse(t *testing.T) {
	c, srv := newSocialRTATestClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	keep := &interfaceSocialHandler{data: "keep"}
	remove := &interfaceSocialHandler{data: "remove"}
	for _, h := range []SubscriptionHandler{keep, remove} {
		if err := c.Subscribe(ctx, h); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Unsubscribe(ctx, remove); err != nil {
		t.Fatal(err)
	}
	if srv.subscribeCount.Load() != 1 || srv.unsubscribeCount.Load() != 0 {
		t.Fatal("removing one handler changed the shared RTA subscription")
	}

	srv.unsubscribeStatus.Store(rta.StatusServiceUnavailable)
	if err := c.Unsubscribe(ctx, keep); err == nil {
		t.Fatal("expected the RTA unsubscribe failure")
	}
	if len(c.subscriptionHandlers) != 0 || !c.subscription.Active() {
		t.Fatal("failed teardown must remove the handler but preserve the active subscription")
	}
	srv.unsubscribeStatus.Store(rta.StatusOK)
	if err := c.Unsubscribe(ctx, keep); err != nil {
		t.Fatalf("retry teardown: %v", err)
	}
	if c.subscription.Active() || srv.unsubscribeCount.Load() != 2 {
		t.Fatal("retry did not release the orphaned RTA subscription")
	}
	if err := c.Unsubscribe(ctx, keep); err != nil {
		t.Fatal(err)
	}
	if srv.unsubscribeCount.Load() != 2 {
		t.Fatal("repeated cleanup sent another RTA request")
	}

	if err := c.Subscribe(ctx, keep); err != nil {
		t.Fatalf("reuse client: %v", err)
	}
	if !c.subscription.Active() || srv.subscribeCount.Load() != 2 {
		t.Fatal("client did not recreate its subscription after cleanup")
	}
	if err := c.CloseContext(ctx); err != nil {
		t.Fatal(err)
	}
	if c.subscription.Active() || len(c.subscriptionHandlers) != 0 || srv.unsubscribeCount.Load() != 3 {
		t.Fatal("CloseContext did not release the reused subscription")
	}
}

func TestUnsubscribeRemovesOneDuplicateRegistration(t *testing.T) {
	c, srv := newSocialRTATestClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h := &interfaceSocialHandler{data: "duplicate"}
	for range 2 {
		if err := c.Subscribe(ctx, h); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Unsubscribe(ctx, h); err != nil {
		t.Fatal(err)
	}
	if len(c.subscriptionHandlers) != 1 || srv.unsubscribeCount.Load() != 0 {
		t.Fatal("removing one registration released the other")
	}
	if err := c.Unsubscribe(ctx, h); err != nil {
		t.Fatal(err)
	}
	if len(c.subscriptionHandlers) != 0 || srv.unsubscribeCount.Load() != 1 {
		t.Fatal("removing the second registration did not release the subscription")
	}
}

func TestConcurrentHandlerRemovalPreservesSharedSubscription(t *testing.T) {
	c, srv := newSocialRTATestClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	keep := &interfaceSocialHandler{data: "keep"}
	if err := c.Subscribe(ctx, keep); err != nil {
		t.Fatal(err)
	}
	dispatch := &subscriptionHandler{Client: c, log: c.log}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for id := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h := &interfaceSocialHandler{data: id}
			for range 16 {
				if err := c.Subscribe(ctx, h); err != nil {
					errs <- err
					return
				}
				dispatch.HandleEvent(json.RawMessage(`{"NotificationType":"Added","Xuids":["1"]}`))
				if err := c.Unsubscribe(ctx, h); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if len(c.subscriptionHandlers) != 1 || c.subscriptionHandlers[0] != keep {
		t.Fatal("concurrent removal changed the persistent handler")
	}
	if srv.subscribeCount.Load() != 1 || srv.unsubscribeCount.Load() != 0 {
		t.Fatal("concurrent handlers replaced the shared RTA subscription")
	}
	if err := c.Unsubscribe(ctx, keep); err != nil {
		t.Fatal(err)
	}
	if srv.unsubscribeCount.Load() != 1 || c.subscription.Active() {
		t.Fatal("final removal did not release the shared subscription")
	}
}

// socialRTATestServer supplies local RTA handshakes for social lifecycle tests.
type socialRTATestServer struct {
	subscribeCount    atomic.Uint32
	unsubscribeCount  atomic.Uint32
	unsubscribeStatus atomic.Int32
}

// newSocialRTATestClient connects the real RTA client to a local test server.
func newSocialRTATestClient(t *testing.T) (*Client, *socialRTATestServer) {
	t.Helper()
	srv := &socialRTATestServer{}
	server := httptest.NewServer(http.HandlerFunc(srv.handle))
	t.Cleanup(server.Close)
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(transport.CloseIdleConnections)
	httpClient := &http.Client{Transport: socialRTATestTransport{target: target, base: transport}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := rta.Dial(ctx, httpClient, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return New(httpClient, conn, xsts.UserInfo{XUID: "1"}, log), srv
}

// handle responds to subscribe and unsubscribe messages over the test WebSocket.
func (s *socialRTATestServer) handle(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols: []string{r.Header.Get("Sec-WebSocket-Protocol")},
	})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	for {
		var request []json.RawMessage
		if err := wsjson.Read(r.Context(), conn, &request); err != nil || len(request) < 2 {
			return
		}
		var typ, seq uint32
		if json.Unmarshal(request[0], &typ) != nil || json.Unmarshal(request[1], &seq) != nil {
			return
		}
		var response []any
		switch typ {
		case 1: // RTA subscribe.
			response = []any{typ, seq, rta.StatusOK, s.subscribeCount.Add(1), map[string]any{}}
		case 2: // RTA unsubscribe.
			s.unsubscribeCount.Add(1)
			response = []any{typ, seq, s.unsubscribeStatus.Load()}
		default:
			return
		}
		if err := wsjson.Write(r.Context(), conn, response); err != nil {
			return
		}
	}
}

// socialRTATestTransport routes the Xbox WebSocket handshake to the local server.
type socialRTATestTransport struct {
	target *url.URL
	base   http.RoundTripper
}

// RoundTrip clones the request before replacing its destination for this test.
func (t socialRTATestTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.URL.Scheme, r.URL.Host = t.target.Scheme, t.target.Host
	r.Host = t.target.Host
	return t.base.RoundTrip(r)
}
