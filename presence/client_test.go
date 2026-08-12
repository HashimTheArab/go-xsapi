package presence

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/df-mc/go-xsapi/v2/xal/xsts"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestConcurrentCloseContextRemovesOnce(t *testing.T) {
	deleteStarted := make(chan struct{})
	releaseDelete := make(chan struct{})
	var deletes atomic.Int32
	client := New(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.Method {
		case http.MethodPost:
			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: req}, nil
		case http.MethodDelete:
			if deletes.Add(1) == 1 {
				close(deleteStarted)
				<-releaseDelete
			}
			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: req}, nil
		default:
			return nil, fmt.Errorf("unexpected method %s", req.Method)
		}
	})}, xsts.UserInfo{XUID: "1234"})
	if _, err := client.Update(context.Background(), TitleRequest{}); err != nil {
		t.Fatalf("Update returned error: %v", err)
	}

	errs := make(chan error, 2)
	go func() { errs <- client.CloseContext(context.Background()) }()
	<-deleteStarted
	go func() { errs <- client.CloseContext(context.Background()) }()
	close(releaseDelete)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("CloseContext returned error: %v", err)
		}
	}
	if got := deletes.Load(); got != 1 {
		t.Fatalf("DELETE requests = %d, want 1", got)
	}
}

func TestUpdateRacingRemoveRetainsNewCleanup(t *testing.T) {
	deleteStarted := make(chan struct{})
	releaseDelete := make(chan struct{})
	var posts, deletes atomic.Int32
	client := New(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.Method {
		case http.MethodPost:
			posts.Add(1)
		case http.MethodDelete:
			if deletes.Add(1) == 1 {
				close(deleteStarted)
				<-releaseDelete
			}
		default:
			return nil, fmt.Errorf("unexpected method %s", req.Method)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: req}, nil
	})}, xsts.UserInfo{XUID: "1234"})
	if _, err := client.Update(context.Background(), TitleRequest{}); err != nil {
		t.Fatalf("initial Update returned error: %v", err)
	}

	removeDone := make(chan error, 1)
	go func() { removeDone <- client.Remove(context.Background()) }()
	<-deleteStarted
	updateDone := make(chan error, 1)
	go func() {
		_, err := client.Update(context.Background(), TitleRequest{})
		updateDone <- err
	}()
	if got := posts.Load(); got != 1 {
		t.Fatalf("POST requests while DELETE is blocked = %d, want 1", got)
	}
	close(releaseDelete)
	if err := <-removeDone; err != nil {
		t.Fatalf("Remove returned error: %v", err)
	}
	if err := <-updateDone; err != nil {
		t.Fatalf("racing Update returned error: %v", err)
	}
	if err := client.CloseContext(context.Background()); err != nil {
		t.Fatalf("CloseContext returned error: %v", err)
	}
	if got := deletes.Load(); got != 2 {
		t.Fatalf("DELETE requests = %d, want 2", got)
	}
}

func TestUpdateReturnsResult(t *testing.T) {
	tests := []struct {
		name      string
		header    string
		heartbeat time.Duration
	}{
		{name: "returns heartbeat header", header: "17", heartbeat: 17 * time.Second},
		{name: "missing heartbeat header returns zero"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := New(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				header := make(http.Header)
				if tt.header != "" {
					header.Set("X-Heartbeat-After", tt.header)
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Status:     "200 OK",
					Header:     header,
					Body:       http.NoBody,
					Request:    req,
				}, nil
			})}, xsts.UserInfo{XUID: "1234"})

			var result *UpdateResult
			result, err := client.Update(context.Background(), TitleRequest{
				State: StateActive,
			})
			if err != nil {
				t.Fatalf("Update returned error: %v", err)
			}
			if result.HeartbeatAfter != tt.heartbeat {
				t.Fatalf("heartbeat = %v, want %v", result.HeartbeatAfter, tt.heartbeat)
			}
		})
	}
}

func TestKeepAliveUsesServerHeartbeat(t *testing.T) {
	var posts atomic.Int32
	client := New(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodDelete {
			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: req}, nil
		}
		if req.Method != http.MethodPost {
			return nil, fmt.Errorf("unexpected method %s", req.Method)
		}
		header := make(http.Header)
		if posts.Add(1) == 1 {
			header.Set("X-Heartbeat-After", "1")
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     header,
			Body:       http.NoBody,
			Request:    req,
		}, nil
	})}, xsts.UserInfo{XUID: "1234"})

	if err := client.KeepAlive(context.Background(), TitleRequest{State: StateActive}); err != nil {
		t.Fatalf("KeepAlive returned error: %v", err)
	}
	if got := posts.Load(); got != 2 {
		t.Fatalf("POST requests = %d, want 2", got)
	}
	if err := client.CloseContext(context.Background()); err != nil {
		t.Fatalf("CloseContext returned error: %v", err)
	}
}

func TestKeepAliveStopsWhileWaitingWhenContextCanceled(t *testing.T) {
	updated := make(chan struct{})
	var posts atomic.Int32
	client := New(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		posts.Add(1)
		select {
		case <-updated:
		default:
			close(updated)
		}
		header := make(http.Header)
		header.Set("X-Heartbeat-After", "60")
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     header,
			Body:       http.NoBody,
			Request:    req,
		}, nil
	})}, xsts.UserInfo{XUID: "1234"})

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- client.KeepAlive(ctx, TitleRequest{State: StateActive})
	}()
	<-updated
	cancel()

	if err := <-errCh; err != context.Canceled {
		t.Fatalf("KeepAlive error = %v, want context.Canceled", err)
	}
	if got := posts.Load(); got != 1 {
		t.Fatalf("POST requests = %d, want 1", got)
	}
}
