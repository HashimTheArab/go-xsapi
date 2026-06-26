package sisu

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// newResponse builds a synthetic *http.Response carrying the provided headers
// and (optionally) a JSON body. It mirrors what a SISU endpoint would return so
// the resilience helpers can be exercised without real network traffic.
func newResponse(status int, header http.Header, body string) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{
		StatusCode: status,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestClusterAffinity(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		value   string // empty means header absent
		wantURL string // empty means nil URL returned
		wantErr string // empty means no error
	}{
		{
			name:    "absent falls back to default",
			value:   "",
			wantURL: "",
			wantErr: "",
		},
		{
			name:    "valid https url",
			value:   "https://sisu-emea.xboxlive.com",
			wantURL: "https://sisu-emea.xboxlive.com",
			wantErr: "",
		},
		{
			name:    "non https rejected",
			value:   "http://sisu-emea.xboxlive.com",
			wantErr: "not https",
		},
		{
			name:    "garbage rejected",
			value:   "::not a url::",
			wantErr: "invalid",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			header := http.Header{}
			if tc.value != "" {
				header.Set(clusterAffinityHeader, tc.value)
			}
			got, err := clusterAffinity(header)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("clusterAffinity error = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("clusterAffinity unexpected error: %v", err)
			}
			if tc.wantURL == "" {
				if got != nil {
					t.Fatalf("clusterAffinity = %v, want nil URL", got)
				}
				return
			}
			if got == nil || got.String() != tc.wantURL {
				t.Fatalf("clusterAffinity = %v, want %q", got, tc.wantURL)
			}
		})
	}
}

func TestSessionEndpointRoutesToCluster(t *testing.T) {
	t.Parallel()

	s := &Session{}
	// With no cluster affinity recorded, the default SISU host is used.
	if got := s.endpoint().Host; got != endpoint.Host {
		t.Fatalf("default endpoint host = %q, want %q", got, endpoint.Host)
	}

	// After recording a valid https cluster affinity, follow-up requests must
	// route to that cluster host instead of the default.
	cluster, err := clusterAffinity(http.Header{clusterAffinityHeader: []string{"https://sisu-emea.xboxlive.com"}})
	if err != nil {
		t.Fatalf("clusterAffinity: %v", err)
	}
	s.setCluster(cluster)
	if got := s.endpoint().Host; got != "sisu-emea.xboxlive.com" {
		t.Fatalf("clustered endpoint host = %q, want %q", got, "sisu-emea.xboxlive.com")
	}
}

func TestIsRetryableXErr(t *testing.T) {
	t.Parallel()

	if !isRetryableXErr(errorCodeSPOP) {
		t.Fatalf("isRetryableXErr(SPOP) = false, want true")
	}
	// A terminal error such as account suspension must never be retried.
	if isRetryableXErr(ErrorCodeAccountSuspended) {
		t.Fatalf("isRetryableXErr(AccountSuspended) = true, want false")
	}
}

func TestAuthorizeWithRetrySPOPRetriesOnce(t *testing.T) {
	t.Parallel()

	spopHeader := http.Header{}
	spopHeader.Set("X-Err", strconv.FormatUint(uint64(errorCodeSPOP), 10))

	okResp := newResponse(http.StatusOK, nil, `{"ok":true}`)

	var attempts int
	s := &Session{}
	resp, err := s.authorizeWithRetry(context.Background(), func(context.Context) (*http.Response, error) {
		attempts++
		if attempts == 1 {
			return newResponse(http.StatusUnauthorized, spopHeader, ""), nil
		}
		return okResp, nil
	})
	if err != nil {
		t.Fatalf("authorizeWithRetry error: %v", err)
	}
	if resp != okResp {
		t.Fatalf("authorizeWithRetry returned the wrong response")
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want exactly 2 (one retry)", attempts)
	}
}

func TestAuthorizeWithRetryNonRetryableFailsFast(t *testing.T) {
	t.Parallel()

	suspendedHeader := http.Header{}
	suspendedHeader.Set("X-Err", strconv.FormatUint(uint64(ErrorCodeAccountSuspended), 10))

	var attempts int
	s := &Session{}
	resp, err := s.authorizeWithRetry(context.Background(), func(context.Context) (*http.Response, error) {
		attempts++
		return newResponse(http.StatusUnauthorized, suspendedHeader, ""), nil
	})
	if err != nil {
		t.Fatalf("authorizeWithRetry transport error: %v", err)
	}
	// The non-retryable response is handed back to the caller for normal error
	// processing; the loop must not retry it.
	if resp == nil {
		t.Fatal("authorizeWithRetry returned nil response")
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want exactly 1 (no retry)", attempts)
	}
}

func TestAuthorizeWithRetryBoundedToSingleRetry(t *testing.T) {
	t.Parallel()

	spopHeader := http.Header{}
	spopHeader.Set("X-Err", strconv.FormatUint(uint64(errorCodeSPOP), 10))

	var attempts int
	s := &Session{}
	// Every attempt keeps returning a retryable SPOP error; the loop must give
	// up after a single retry rather than spinning forever.
	resp, err := s.authorizeWithRetry(context.Background(), func(context.Context) (*http.Response, error) {
		attempts++
		return newResponse(http.StatusUnauthorized, spopHeader, ""), nil
	})
	if err != nil {
		t.Fatalf("authorizeWithRetry transport error: %v", err)
	}
	if resp == nil {
		t.Fatal("authorizeWithRetry returned nil response")
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want exactly 2 (bounded single retry)", attempts)
	}
}

func TestAuthorizeWithRetryPropagatesTransportError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom")
	s := &Session{}
	_, err := s.authorizeWithRetry(context.Background(), func(context.Context) (*http.Response, error) {
		return nil, wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("authorizeWithRetry error = %v, want %v", err, wantErr)
	}
}
