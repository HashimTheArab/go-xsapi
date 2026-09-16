package xsapi

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestRESTClientSignsFinalJSONRequest(t *testing.T) {
	key := mustGenerateECDSAKey(t)
	token := testXSTSToken(time.Now().Add(time.Hour))
	token.DisplayClaims.UserInfo[0].XUID = "123"
	src := &recordingTokenSource{token: token, proofKey: key}
	requests := 0
	httpClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		switch req.URL.Host {
		case "title.mgt.xboxlive.com":
			return nsalTitleDataResponse("*.xboxlive.com", "http://xboxlive.com"), nil
		case "social.xboxlive.com":
			body, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatal(err)
			}
			var data struct {
				XUIDs []string `json:"xuids"`
			}
			if err := json.Unmarshal(body, &data); err != nil || len(data.XUIDs) != 1 || data.XUIDs[0] != "456" {
				t.Fatalf("invalid request JSON: %s (%v)", body, err)
			}
			if req.Header.Get("X-Custom") != "option" || req.Header.Get("X-Xbl-Contract-Version") != "3" {
				t.Fatal("request options did not reach the authenticated transport")
			}
			if _, ok := req.Context().Deadline(); !ok {
				t.Fatal("REST request has no deadline in its signing transport")
			}
			verifyRESTSignature(t, req, body, &key.PublicKey)
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"updatedPeople":["456"]}`)), Request: req}, nil
		default:
			t.Fatalf("unexpected request: %s", req.URL)
			return nil, nil
		}
	})}
	client, err := (ClientConfig{HTTPClient: httpClient, RTAMode: RTADisabled}).New(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := client.Social().AddFriends(context.Background(), []string{"456"}, RequestHeader("X-Custom", "option"))
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 || len(updated) != 1 || updated[0] != "456" {
		t.Fatalf("requests=%d updated=%v", requests, updated)
	}
	if httpClient.Timeout != 0 {
		t.Fatal("REST setup mutated the supplied HTTP client")
	}
}

// verifyRESTSignature checks that the signature covers the method, URL,
// authorization, and exact JSON bytes handed to the final HTTP transport.
func verifyRESTSignature(t *testing.T, req *http.Request, body []byte, key *ecdsa.PublicKey) {
	t.Helper()
	signature, err := base64.StdEncoding.DecodeString(req.Header.Get("Signature"))
	if err != nil || len(signature) != 76 || req.Header.Get("Authorization") == "" {
		t.Fatalf("missing or malformed authentication signature: %v", err)
	}
	hash := sha256.New()
	for _, part := range [][]byte{
		signature[:4], signature[4:12], []byte(req.Method),
		[]byte(req.URL.RequestURI()), []byte(req.Header.Get("Authorization")), body,
	} {
		_, _ = hash.Write(part)
		_, _ = hash.Write([]byte{0})
	}
	if !ecdsa.Verify(key, hash.Sum(nil), new(big.Int).SetBytes(signature[12:44]), new(big.Int).SetBytes(signature[44:])) {
		t.Fatal("signature does not cover the transmitted request")
	}
}
