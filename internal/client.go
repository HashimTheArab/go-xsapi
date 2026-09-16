package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-resty/resty/v2"
)

// XBLRelyingParty is the relying party used for various Xbox Live services.
// In XSAPI Client, it will be used for requesting NSAL endpoints for current
// authenticated title.
const XBLRelyingParty = "http://xboxlive.com"

const (
	defaultRESTTimeout  = 30 * time.Second
	maxResponseBodySize = 16 << 20
)

type requestOptionsKey struct{}

// NewRESTClient wraps a copy of client without changing its transport, cookie
// jar, or redirect policy. A missing timeout becomes 30 seconds. Response bodies,
// including errors and decompressed data, are limited to 16 MiB and closed by
// Resty. Automatic retries are disabled because mutations may have taken effect
// even when no response arrives.
func NewRESTClient(client *http.Client) *resty.Client {
	if client == nil {
		client = http.DefaultClient
	}
	cloned := *client
	if cloned.Transport == nil {
		cloned.Transport = http.DefaultTransport
	}
	if cloned.Timeout <= 0 {
		cloned.Timeout = defaultRESTTimeout
	}
	return resty.NewWithClient(&cloned).
		SetResponseBodyLimit(maxResponseBodySize).
		SetRetryCount(0).
		SetPreRequestHook(func(_ *resty.Client, req *http.Request) error {
			opts, _ := req.Context().Value(requestOptionsKey{}).([]RequestOption)
			Apply(req, opts)
			return nil
		})
}

// Request creates a request whose options run after Resty builds the HTTP
// request, but before the authentication transport signs it. Options are stored
// on this request's context so concurrent calls cannot overwrite each other.
func Request(ctx context.Context, client *resty.Client, opts []RequestOption) *resty.Request {
	return client.R().SetContext(context.WithValue(ctx, requestOptionsKey{}, opts))
}

// DecodeJSON decodes a buffered response after the endpoint has checked its
// accepted status codes. Xbox responses are decoded even if Content-Type is
// missing or incorrect, matching the service clients' existing behavior.
func DecodeJSON(resp *resty.Response, dst any) error {
	if err := json.NewDecoder(bytes.NewReader(resp.Body())).Decode(dst); err != nil {
		return fmt.Errorf("decode response body: %w", err)
	}
	return nil
}

// UnexpectedStatusCode describes an unexpected HTTP status code and the request
// that produced it. It must only be called after a successful HTTP exchange.
func UnexpectedStatusCode(resp *resty.Response) error {
	req := resp.RawResponse.Request
	return fmt.Errorf("%s %s: %s", req.Method, req.URL, resp.Status())
}

// Do sends a JSON request to an endpoint that accepts 200 OK or 201 Created.
// If respBody is non-nil, the successful response is JSON-decoded into it.
func Do(ctx context.Context, client *resty.Client, method, u string, reqBody, respBody any, opts []RequestOption) error {
	req := Request(ctx, client, opts)
	if reqBody != nil {
		req.SetBody(reqBody).SetHeader("Content-Type", "application/json")
	}
	resp, err := req.Execute(method, u)
	if err != nil {
		return err
	}
	switch resp.StatusCode() {
	case http.StatusOK, http.StatusCreated:
		if respBody != nil {
			return DecodeJSON(resp, respBody)
		}
		return nil
	default:
		return UnexpectedStatusCode(resp)
	}
}
