package mpsd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/df-mc/go-xsapi/v2/xal/xsts"
	"github.com/google/uuid"
)

func TestSessionSyncResponseHandling(t *testing.T) {
	for _, tt := range []struct {
		name       string
		status     int
		body       string
		etag       string
		wantETag   string
		wantUpdate bool
		wantErr    bool
	}{
		{name: "updated without content type", status: http.StatusOK, body: `{"members":{}}`, etag: `"new"`, wantETag: `"new"`, wantUpdate: true},
		{name: "missing etag preserves old value", status: http.StatusOK, body: `{}`, wantETag: `"old"`, wantUpdate: true},
		{name: "not modified", status: http.StatusNotModified, wantETag: `"old"`},
		{name: "invalid json preserves cache", status: http.StatusOK, body: `invalid`, etag: `"new"`, wantETag: `"old"`, wantErr: true},
		{name: "no content is not a sync success", status: http.StatusNoContent, wantETag: `"old"`, wantErr: true},
		{name: "created is not a sync success", status: http.StatusCreated, body: `{}`, wantETag: `"old"`, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body := &joinResponseBody{Reader: strings.NewReader(tt.body)}
			requests := 0
			httpClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				requests++
				if req.Method != http.MethodGet || req.Header.Get("If-None-Match") != `"old"` {
					t.Fatalf("request = %s with If-None-Match %q, want GET with old ETag", req.Method, req.Header.Get("If-None-Match"))
				}
				if req.Header.Get("Accept") != "application/json" || req.Header.Get("X-Xbl-Contract-Version") != contractVersion {
					t.Fatalf("missing session request headers: %v", req.Header)
				}
				header := make(http.Header)
				header.Set("ETag", tt.etag)
				return &http.Response{
					StatusCode: tt.status,
					Status:     fmt.Sprintf("%d %s", tt.status, http.StatusText(tt.status)),
					Header:     header,
					Body:       body,
					Request:    req,
				}, nil
			})}
			session := &Session{
				client: New(httpClient, nil, xsts.UserInfo{}, nil),
				ref:    SessionReference{ServiceConfigID: uuid.New(), TemplateName: "template", Name: "SESSION"},
				cache:  SessionDescription{Members: map[string]*MemberDescription{"removed": {}}},
				etag:   `"old"`,
				closed: make(chan struct{}),
			}
			if err := session.Sync(context.Background()); (err != nil) != tt.wantErr {
				t.Fatalf("Sync error = %v, want error %t", err, tt.wantErr)
			}
			if requests != 1 || !body.closed {
				t.Fatalf("requests = %d, body closed = %t; want 1 and true", requests, body.closed)
			}
			if session.etag != tt.wantETag {
				t.Fatalf("ETag = %q, want %q", session.etag, tt.wantETag)
			}
			if _, retained := session.cache.Members["removed"]; retained == tt.wantUpdate {
				t.Fatalf("old member retained = %t, want %t", retained, !tt.wantUpdate)
			}
		})
	}
}

func TestPublishResponseStatus(t *testing.T) {
	for _, status := range []int{http.StatusCreated, http.StatusOK, http.StatusNoContent} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			connectionID := uuid.New()
			ref := SessionReference{ServiceConfigID: uuid.New(), TemplateName: "template", Name: "SESSION"}
			description, err := json.Marshal(SessionDescription{Members: map[string]*MemberDescription{
				"me": {Properties: &MemberProperties{System: &MemberPropertiesSystem{Active: true, Connection: connectionID}}},
			}})
			if err != nil {
				t.Fatal(err)
			}
			body := &joinResponseBody{Reader: strings.NewReader(string(description))}
			requests := 0
			httpClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				requests++
				if req.Method == http.MethodPost && req.URL.Path == "/handles" {
					return &http.Response{StatusCode: http.StatusCreated, Body: http.NoBody, Request: req}, nil
				}
				if req.Method != http.MethodPut || req.URL.String() != ref.URL().String() {
					return nil, fmt.Errorf("unexpected request: %s %s", req.Method, req.URL)
				}
				if req.Header.Get("If-None-Match") != "*" || req.Header.Get("X-Xbl-Contract-Version") != contractVersion {
					t.Fatalf("missing publish request headers: %v", req.Header)
				}
				return &http.Response{
					StatusCode: status,
					Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
					Header:     http.Header{"Etag": []string{`"published"`}},
					Body:       body,
					Request:    req,
				}, nil
			})}
			client := newJoinTestClient(httpClient, connectionID)
			session, err := client.Publish(context.Background(), ref, PublishConfig{})
			if status == http.StatusCreated {
				if err != nil {
					t.Fatal(err)
				}
				if !session.Reference().Equal(ref) || session.etag != `"published"` || requests != 2 {
					t.Fatalf("unexpected published session: reference=%v, ETag=%q, requests=%d", session.Reference(), session.etag, requests)
				}
			} else if err == nil || session != nil || requests != 1 {
				t.Fatalf("Publish = %v, %v after %d requests; want an error without activity publication", session, err, requests)
			}
			if !body.closed {
				t.Fatal("publish response body was not closed")
			}
		})
	}
}

func TestJoinDoesNotRetryResponseReadFailure(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusPreconditionFailed} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			readErr := errors.New("response body interrupted")
			body := &joinResponseBody{Reader: iotest.ErrReader(readErr)}
			requests := 0
			client := newJoinTestClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				requests++
				return &http.Response{StatusCode: status, Body: body, Request: req}, nil
			})}, uuid.New())
			_, err := client.Join(context.Background(), uuid.New(), JoinConfig{})
			if !errors.Is(err, readErr) {
				t.Fatalf("Join error = %v, want response read error", err)
			}
			if requests != 1 || !body.closed {
				t.Fatalf("requests = %d, body closed = %t; want 1 and true", requests, body.closed)
			}
		})
	}
}
