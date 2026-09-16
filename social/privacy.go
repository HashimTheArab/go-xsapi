package social

import (
	"context"
	"net/http"
	"net/url"

	"github.com/df-mc/go-xsapi/v2/internal"
)

// Block blocks the user identified by the given XUID.
func (c *Client) Block(ctx context.Context, xuid string, opts ...internal.RequestOption) error {
	return c.updatePrivacy(ctx, http.MethodPut, xuid, "never", append(opts,
		internal.ContractVersion("1"),
	))
}

// Unblock unblocks the user identified by the given XUID.
func (c *Client) Unblock(ctx context.Context, xuid string, opts ...internal.RequestOption) error {
	return c.updatePrivacy(ctx, http.MethodDelete, xuid, "never", append(opts,
		internal.ContractVersion("1"),
	))
}

// Mute mutes the user identified by the given XUID.
func (c *Client) Mute(ctx context.Context, xuid string, opts ...internal.RequestOption) error {
	return c.updatePrivacy(ctx, http.MethodPut, xuid, "mute", append(opts,
		internal.ContractVersion("2"),
	))
}

// Unmute unmutes the user identified by the given XUID.
func (c *Client) Unmute(ctx context.Context, xuid string, opts ...internal.RequestOption) error {
	return c.updatePrivacy(ctx, http.MethodDelete, xuid, "mute", append(opts,
		internal.ContractVersion("2"),
	))
}

// Blocked returns a list of XUIDs whose are blocked by the caller.
func (c *Client) Blocked(ctx context.Context, opts ...internal.RequestOption) (xuids []string, err error) {
	return c.listPrivacy(ctx, "never", opts)
}

// Muted returns a list of XUIDs whose are muted by the caller.
func (c *Client) Muted(ctx context.Context, opts ...internal.RequestOption) (xuids []string, err error) {
	return c.listPrivacy(ctx, "mute", opts)
}

// updatePrivacy updates the specific privacy setting for the user identified by the given XUID.
// The callers must specify the appropriate contract version depending on the privacy setting used for update.
func (c *Client) updatePrivacy(ctx context.Context, method, xuid, typ string, opts []internal.RequestOption) error {
	requestURL := privacyEndpoint.JoinPath("/users/xuid("+c.userInfo.XUID+")/people", typ).String()
	resp, err := internal.Request(ctx, c.client, append(opts,
		internal.DefaultLanguage,
		internal.RequestHeader("Content-Type", "application/json"),
		internal.RequestHeader("Accept", "application/json"),
		internal.RequestHeader("Cache-Control", "no-cache"),
	)).SetBody(map[string]any{"Xuid": xuid}).Execute(method, requestURL)
	if err != nil {
		return err
	}
	if resp.StatusCode() != http.StatusOK {
		return internal.UnexpectedStatusCode(resp)
	}
	return nil
}

// listPrivacy lists XUIDs whose are affected by the caller's privacy settings for the given type.
func (c *Client) listPrivacy(ctx context.Context, typ string, opts []internal.RequestOption) ([]string, error) {
	requestURL := privacyEndpoint.JoinPath("/users/xuid("+c.userInfo.XUID+")/people", typ).String()
	resp, err := internal.Request(ctx, c.client, append(opts,
		internal.DefaultLanguage,
		internal.ContractVersion("1"),
		internal.RequestHeader("Content-Type", "application/json"),
		internal.RequestHeader("Accept", "application/json"),
		internal.RequestHeader("Cache-Control", "no-cache"),
	)).Get(requestURL)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode() != http.StatusOK {
		return nil, internal.UnexpectedStatusCode(resp)
	}
	var list privacyList
	if err := internal.DecodeJSON(resp, &list); err != nil {
		return nil, err
	}
	if len(list.Users) == 0 {
		return nil, nil
	}
	xuids := make([]string, len(list.Users))
	for i, entry := range list.Users {
		xuids[i] = entry.XUID
	}
	return xuids, nil
}

type (
	// privacyList represents the response returned by [Client.listPrivacy].
	privacyList struct {
		// Users lists the users affected by the caller's privacy settings for a specific type.
		Users []privacyListEntry `json:"users"`
	}
	// privacyListEntry represents a single user in the privacyList.
	privacyListEntry struct {
		// XUID is the XUID of the user.
		XUID string `json:"xuid"`
	}
)

var (
	// privacyEndpoint is the base URL used to make requests to the Xbox Live Privacy API.
	//
	// Requests sent to this endpoint must include the 'X-Xbl-Contract-Version'
	// header set to '1' or '2' depending on the endpoint.
	privacyEndpoint = &url.URL{
		Scheme: "https",
		Host:   "privacy.xboxlive.com",
	}
)
