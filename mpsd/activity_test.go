package mpsd

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

func TestActivitiesOmitsSocialGroupFilter(t *testing.T) {
	var requestBody map[string]any
	client := &Client{
		client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			defer req.Body.Close()
			if err := json.NewDecoder(req.Body).Decode(&requestBody); err != nil {
				t.Fatalf("decode request body: %v", err)
			}
			return testResponse(req, http.StatusOK, nil, []byte(`{"results":[]}`)), nil
		})},
	}

	if _, err := client.Activities(context.Background(), uuid.New()); err != nil {
		t.Fatalf("Activities returned error: %v", err)
	}

	owners, ok := requestBody["owners"].(map[string]any)
	if !ok {
		t.Fatalf("owners = %#v, want object", requestBody["owners"])
	}
	if _, ok := owners["people"]; ok {
		t.Fatalf("owners.people = %#v, want omitted", owners["people"])
	}
	if _, ok := owners["xuids"]; ok {
		t.Fatalf("owners.xuids = %#v, want omitted for all-users query", owners["xuids"])
	}
}

func TestActivitiesForUsersEncodesOnlyXUIDFilter(t *testing.T) {
	var requestBody map[string]any
	xuids := []string{"123", "456"}
	client := &Client{
		client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			defer req.Body.Close()
			if err := json.NewDecoder(req.Body).Decode(&requestBody); err != nil {
				t.Fatalf("decode request body: %v", err)
			}
			return testResponse(req, http.StatusOK, nil, []byte(`{"results":[]}`)), nil
		})},
	}

	if _, err := client.ActivitiesForUsers(context.Background(), uuid.New(), xuids); err != nil {
		t.Fatalf("ActivitiesForUsers returned error: %v", err)
	}

	owners, ok := requestBody["owners"].(map[string]any)
	if !ok {
		t.Fatalf("owners = %#v, want object", requestBody["owners"])
	}
	if _, ok := owners["people"]; ok {
		t.Fatalf("owners.people = %#v, want omitted", owners["people"])
	}
	gotXUIDs, ok := owners["xuids"].([]any)
	if !ok {
		t.Fatalf("owners.xuids = %#v, want array", owners["xuids"])
	}
	if len(gotXUIDs) != len(xuids) {
		t.Fatalf("owners.xuids length = %d, want %d", len(gotXUIDs), len(xuids))
	}
	for i, xuid := range xuids {
		if gotXUIDs[i] != xuid {
			t.Fatalf("owners.xuids[%d] = %#v, want %q", i, gotXUIDs[i], xuid)
		}
	}
}
