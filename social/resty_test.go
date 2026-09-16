package social

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/df-mc/go-xsapi/v2/internal"
	"github.com/df-mc/go-xsapi/v2/xal/xsts"
)

func TestUsersByXUIDsKeepsJSONContentType(t *testing.T) {
	client := New(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost || req.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("request=%s Content-Type=%q", req.Method, req.Header.Get("Content-Type"))
		}
		data, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatal(err)
		}
		var batch batchRequest
		if err := json.Unmarshal(data, &batch); err != nil {
			t.Fatal(err)
		}
		if len(batch.XUIDs) != 1 || batch.XUIDs[0] != "123" {
			t.Fatalf("request body=%s", data)
		}
		return response(req, http.StatusOK, `{"people":[]}`), nil
	})}, nil, xsts.UserInfo{}, nil)
	if _, err := client.UsersByXUIDs(context.Background(), []string{"123"}, internal.RequestHeader("Content-Type", "text/plain")); err != nil {
		t.Fatal(err)
	}
}
