package internal

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-resty/resty/v2"
	"golang.org/x/text/language"
)

func TestNewRESTClientDefaultsAndInjection(t *testing.T) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	redirectErr := errors.New("redirect rejected")
	transport := &http.Transport{}
	for _, timeout := range []time.Duration{0, -1, 5 * time.Second} {
		t.Run(timeout.String(), func(t *testing.T) {
			original := &http.Client{
				Transport:     transport,
				Jar:           jar,
				Timeout:       timeout,
				CheckRedirect: func(*http.Request, []*http.Request) error { return redirectErr },
			}
			client := NewRESTClient(original)
			cloned := client.GetClient()
			wantTimeout := timeout
			if wantTimeout <= 0 {
				wantTimeout = 30 * time.Second
			}
			if cloned == original || original.Timeout != timeout || cloned.Timeout != wantTimeout {
				t.Fatalf("client was not cloned with the expected timeout: original=%v cloned=%v", original.Timeout, cloned.Timeout)
			}
			if cloned.Transport != transport || cloned.Jar != jar || cloned.CheckRedirect(nil, nil) != redirectErr {
				t.Fatal("injected transport, cookie jar, or redirect policy was replaced")
			}
			if client.ResponseBodyLimit != 16<<20 || client.RetryCount != 0 {
				t.Fatalf("body limit=%d retries=%d", client.ResponseBodyLimit, client.RetryCount)
			}
		})
	}
	client := NewRESTClient(nil)
	if client.GetClient() == http.DefaultClient || client.GetClient().Transport != http.DefaultTransport {
		t.Fatal("default client was not copied with the standard default transport")
	}
}

func TestRESTRequestDeadlines(t *testing.T) {
	for _, partialBody := range []bool{false, true} {
		for _, callerDeadline := range []bool{false, true} {
			t.Run(fmt.Sprintf("body=%t/caller=%t", partialBody, callerDeadline), func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if partialBody {
						_, _ = io.WriteString(w, `{"partial":`)
						w.(http.Flusher).Flush()
					}
					<-r.Context().Done()
				}))
				defer server.Close()
				ctx := context.Background()
				httpClient := server.Client()
				if callerDeadline {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, 50*time.Millisecond)
					defer cancel()
				} else {
					httpClient.Timeout = 50 * time.Millisecond
				}
				_, err := Request(ctx, NewRESTClient(httpClient), nil).Get(server.URL)
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("error=%v, want deadline exceeded", err)
				}
			})
		}
	}
}

func TestRESTResponseLimitAndClose(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusTooManyRequests} {
		for _, compressed := range []bool{false, true} {
			t.Run(fmt.Sprintf("status=%d/gzip=%t", status, compressed), func(t *testing.T) {
				data := bytes.Repeat([]byte("a"), 128)
				header := make(http.Header)
				if compressed {
					var buf bytes.Buffer
					writer := gzip.NewWriter(&buf)
					if _, err := writer.Write(data); err != nil {
						t.Fatal(err)
					}
					if err := writer.Close(); err != nil {
						t.Fatal(err)
					}
					data = buf.Bytes()
					header.Set("Content-Encoding", "gzip")
				}
				body := &trackedBody{Reader: bytes.NewReader(data)}
				client := NewRESTClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: status, Header: header, Body: body, ContentLength: -1, Request: req}, nil
				})}).SetResponseBodyLimit(32)
				_, err := Request(context.Background(), client, nil).Get("https://example.com")
				if !errors.Is(err, resty.ErrResponseBodyTooLarge) || !body.closed {
					t.Fatalf("error=%v closed=%t, want size error and closed body", err, body.closed)
				}
			})
		}
	}
}

func TestRESTDoesNotRetryMutations(t *testing.T) {
	transportErr := errors.New("connection lost after write")
	for _, status := range []int{0, http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			calls := 0
			body := &trackedBody{Reader: strings.NewReader(`{}`)}
			client := NewRESTClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				if status == 0 {
					return nil, transportErr
				}
				return &http.Response{StatusCode: status, Body: body, Request: req}, nil
			})})
			resp, err := Request(context.Background(), client, nil).SetBody(map[string]string{"xuid": "1"}).Post("https://example.com")
			if calls != 1 {
				t.Fatalf("requests=%d, want 1", calls)
			}
			if status == 0 {
				if !errors.Is(err, transportErr) {
					t.Fatalf("error=%v, want transport error", err)
				}
			} else if err != nil || resp.StatusCode() != status || !body.closed {
				t.Fatalf("response=%v error=%v closed=%t", resp, err, body.closed)
			}
		})
	}
}

func TestRESTRequestOptionsAreIsolated(t *testing.T) {
	client := NewRESTClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if got := req.Header.Values("Accept-Language"); strings.Join(got, ";") != "fr;en-US, en" {
			return nil, fmt.Errorf("language options out of order: %v", got)
		}
		if req.Header.Get("X-ID") != req.URL.Query().Get("id") || req.Header.Get("X-Override") != "last" {
			return nil, fmt.Errorf("request options mixed between calls: %s %v", req.URL, req.Header)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: req}, nil
	})})
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			id := strconv.Itoa(i)
			opts := []RequestOption{
				RequestHeader("X-ID", id), nil,
				RequestHeader("X-Override", "first"), RequestHeader("X-Override", "last"),
				AcceptLanguage([]language.Tag{language.French}), DefaultLanguage,
				func(req *http.Request) { req.URL.RawQuery = "id=" + id },
			}
			if _, err := Request(context.Background(), client, opts).Get("https://example.com"); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
}

func TestDoResponseHandling(t *testing.T) {
	for _, tt := range []struct {
		name    string
		status  int
		body    string
		wantErr bool
	}{
		{name: "ok without content type", status: 200, body: `{"value":1}`},
		{name: "created", status: 201, body: `{"value":1}`},
		{name: "accepted is not supported", status: 202, body: `invalid`, wantErr: true},
		{name: "empty json", status: 200, wantErr: true},
		{name: "malformed json", status: 200, body: `invalid`, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body := &trackedBody{Reader: strings.NewReader(tt.body)}
			client := NewRESTClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tt.status, Status: http.StatusText(tt.status), Body: body, Request: req}, nil
			})})
			var result struct{ Value int }
			err := Do(context.Background(), client, http.MethodGet, "https://example.com", nil, &result, nil)
			if (err != nil) != tt.wantErr || !body.closed {
				t.Fatalf("error=%v closed=%t", err, body.closed)
			}
			if err == nil && result.Value != 1 {
				t.Fatalf("decoded value=%d, want 1", result.Value)
			}
			if tt.status == 202 && !strings.Contains(err.Error(), "Accepted") {
				t.Fatalf("unexpected status was decoded: %v", err)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

// RoundTrip calls the test transport function.
func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type trackedBody struct {
	io.Reader
	closed bool
}

// Close records that the response body has been released.
func (b *trackedBody) Close() error { b.closed = true; return nil }
