package sisu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"

	"github.com/df-mc/go-xsapi/v2/xal"
	"golang.org/x/oauth2"
)

// credentialsRedirectURI is the "native client" redirect used by the headless
// credentials flow. Unlike [Config.RedirectURI] (which typically uses an
// ms-xal:// scheme handled inside a WebView), the desktop redirect keeps the
// final authorization code readable directly from the request URL.
const credentialsRedirectURI = "https://login.live.com/oauth20_desktop.srf"

var (
	// credentialsAuthorizeURL is the Microsoft "LIVE" authorize endpoint used by
	// [Config.CredentialsToken]. It is a package-level variable (rather than a
	// const) solely so tests can point the flow at a local httptest server; in
	// production it always uses the real Live endpoint.
	credentialsAuthorizeURL = "https://login.live.com/oauth20_authorize.srf"

	// credentialsTokenURL, when non-empty, overrides the Live token endpoint used
	// when exchanging the authorization code. It exists only so tests can redirect
	// the token exchange to a local server; production leaves it empty and uses
	// the endpoint from Config.oauth2.
	credentialsTokenURL string
)

// CredentialsToken performs a headless Microsoft-account login using the given
// email and password and returns an [oauth2.Token] usable with the rest of the
// SISU/Xbox Live flow (for example via [Config.TokenSource] or [Config.New]).
//
// It ports RaphiMC/MinecraftAuth's CredentialsMsaAuthService for the "LIVE"
// (login.live.com) environment: it scrapes the login form served by
// oauth20_authorize.srf, submits the credentials, follows the redirect back to
// the desktop redirect URI, and exchanges the resulting authorization code.
//
// WARNING: this flow is fragile. It scrapes and drives Microsoft's HTML login
// pages, so it breaks whenever the account requires two-factor authentication,
// a CAPTCHA, an interrupt/consent page, or conditional-access policies, and it
// may break without notice if Microsoft changes those pages. Prefer the
// device-code flow ([Config.DeviceAuth] / [Config.DeviceAccessToken]) or the
// authorization-code flow ([Config.AuthCodeURL] / [Config.Exchange]) whenever an
// interactive login is possible. Use CredentialsToken only for headless,
// non-interactive accounts that you control.
func (conf Config) CredentialsToken(ctx context.Context, email, password string) (*oauth2.Token, error) {
	client, err := jarClient(oauth2Client(ctx))
	if err != nil {
		return nil, fmt.Errorf("xal/sisu: build cookie client: %w", err)
	}
	code, err := conf.credentialsAuthCode(ctx, client, email, password)
	if err != nil {
		return nil, err
	}
	token, err := conf.credentialsExchange(ctx, code)
	if err != nil {
		return nil, fmt.Errorf("xal/sisu: exchange authorization code: %w", err)
	}
	return token, nil
}

// jarClient returns a shallow copy of base with an in-memory cookie jar
// installed. The underlying Transport and Timeout are preserved so the caller's
// client configuration (proxy, TLS, deadline) is respected; base is never
// mutated. The jar is required so the session established by the authorize
// request is carried into the login POST and the subsequent redirects.
func jarClient(base *http.Client) (*http.Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	cloned := *base
	cloned.Jar = jar
	return &cloned, nil
}

// credentialsAuthCode drives the HTML login flow and returns the authorization
// code read from the final desktop-redirect URL. client must have a cookie jar.
func (conf Config) credentialsAuthCode(ctx context.Context, client *http.Client, email, password string) (string, error) {
	// 1. GET the authorize page to obtain the login form (urlPost + PPFT).
	authorizeURL := credentialsAuthorizeURL + "?" + url.Values{
		"client_id":     {conf.ClientID},
		"scope":         {scope},
		"redirect_uri":  {credentialsRedirectURI},
		"response_type": {"code"},
		"response_mode": {"query"},
	}.Encode()

	data, err := conf.fetchLoginForm(ctx, client, authorizeURL)
	if err != nil {
		return "", err
	}
	if data.URLPost == "" {
		return "", errors.New("xal/sisu: authorize response missing urlPost")
	}
	name, value, err := parsePPFT(data.SFTTag)
	if err != nil {
		return "", fmt.Errorf("xal/sisu: parse PPFT token: %w", err)
	}

	// 2. POST the credentials to urlPost. The client follows the resulting
	// redirects (carrying the session cookies) until it lands on the desktop
	// redirect URI containing the authorization code.
	form := url.Values{
		"login":    {email},
		"loginfmt": {email},
		"passwd":   {password},
		name:       {value},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, data.URLPost, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("xal/sisu: make login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "text/html")
	if conf.UserAgent != "" {
		req.Header.Set("User-Agent", conf.UserAgent)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("xal/sisu: send login request: %w", err)
	}
	defer resp.Body.Close()

	// 3. Read the authorization code from the final URL, or surface a clear
	// error explaining why the login did not complete.
	if code, ok := codeFromURL(resp.Request.URL); ok {
		return code, nil
	}
	if err := redirectError(resp.Request.URL); err != nil {
		return "", err
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return "", loginError(string(body))
}

// fetchLoginForm performs the authorize GET and extracts the embedded
// ServerData configuration from the returned login page. If the request instead
// redirects back with an OAuth error, that error is surfaced.
func (conf Config) fetchLoginForm(ctx context.Context, client *http.Client, rawURL string) (*serverData, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("xal/sisu: make authorize request: %w", err)
	}
	req.Header.Set("Accept", "text/html")
	if conf.UserAgent != "" {
		req.Header.Set("User-Agent", conf.UserAgent)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("xal/sisu: send authorize request: %w", err)
	}
	defer resp.Body.Close()

	if err := redirectError(resp.Request.URL); err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, xal.UnexpectedStatus(resp)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("xal/sisu: read authorize response: %w", err)
	}
	data, err := extractServerData(string(raw))
	if err != nil {
		return nil, fmt.Errorf("xal/sisu: %w", err)
	}
	return data, nil
}

// credentialsExchange exchanges the authorization code obtained from the headless
// login for an [oauth2.Token]. It mirrors [Config.Exchange] (adding the scope and
// client_id parameters) but pins redirect_uri to the desktop redirect used during
// the login so Microsoft accepts the exchange. The token endpoint may be
// overridden by tests via credentialsTokenURL; production uses the Live endpoint.
func (conf Config) credentialsExchange(ctx context.Context, code string) (*oauth2.Token, error) {
	oc := conf.oauth2()
	if credentialsTokenURL != "" {
		oc.Endpoint.TokenURL = credentialsTokenURL
	}
	return oc.Exchange(oauth2Context(ctx), code,
		oauth2.SetAuthURLParam("scope", scope),
		oauth2.SetAuthURLParam("client_id", conf.ClientID),
		oauth2.SetAuthURLParam("redirect_uri", credentialsRedirectURI),
	)
}

// serverDataMarker precedes the JSON login configuration embedded in the LIVE
// authorize page, as in `var ServerData = {...};`.
const serverDataMarker = "var ServerData = "

// serverData holds the fields of the embedded ServerData JSON object that the
// credentials flow needs. Unknown fields are ignored.
type serverData struct {
	// URLPost is the form action the credentials are submitted to.
	URLPost string `json:"urlPost"`
	// SFTTag is an HTML snippet containing the hidden PPFT flow-token input.
	SFTTag string `json:"sFTTag"`
	// ErrorCode and ErrorText are populated when the page reports a login error.
	ErrorCode string `json:"sErrorCode"`
	ErrorText string `json:"sErrTxt"`
}

// extractServerData locates the `var ServerData = {...}` assignment in the LIVE
// login page and decodes the JSON object that follows it. Mirroring
// MinecraftAuth, it reads the first JSON value after the marker and ignores the
// trailing `;` and any remaining script, rather than relying on a brace-matching
// regex that nested objects would defeat.
func extractServerData(html string) (*serverData, error) {
	i := strings.Index(html, serverDataMarker)
	if i == -1 {
		return nil, errors.New("ServerData not found in login page")
	}
	rest := strings.TrimLeft(html[i+len(serverDataMarker):], " \t\r\n(")
	var data serverData
	if err := json.NewDecoder(strings.NewReader(rest)).Decode(&data); err != nil {
		return nil, fmt.Errorf("decode ServerData: %w", err)
	}
	return &data, nil
}

var (
	// inputValueRe and inputNameRe pull the value="" and name="" attributes out
	// of the hidden PPFT input contained in the sFTTag field.
	inputValueRe = regexp.MustCompile(`value="([^"]*)"`)
	inputNameRe  = regexp.MustCompile(`name="([^"]*)"`)
)

// parsePPFT extracts the flow-token field name and value from the sFTTag HTML
// snippet (a hidden <input>, typically name="PPFT"). Both must be submitted with
// the login POST for it to be accepted.
func parsePPFT(sFTTag string) (name, value string, err error) {
	v := inputValueRe.FindStringSubmatch(sFTTag)
	if v == nil {
		return "", "", errors.New("PPFT value not found in sFTTag")
	}
	n := inputNameRe.FindStringSubmatch(sFTTag)
	if n == nil {
		return "", "", errors.New("PPFT field name not found in sFTTag")
	}
	return n[1], v[1], nil
}

// codeFromURL returns the OAuth authorization code carried in u's query, if any.
func codeFromURL(u *url.URL) (string, bool) {
	if u == nil {
		return "", false
	}
	code := u.Query().Get("code")
	if code == "" {
		return "", false
	}
	return code, true
}

// redirectError returns a descriptive error if u carries an OAuth error in its
// query (for example when the authorize request rejects the client, or the login
// is redirected back with an error), or nil otherwise.
func redirectError(u *url.URL) error {
	if u == nil {
		return nil
	}
	q := u.Query()
	code := q.Get("error")
	if code == "" {
		return nil
	}
	if desc := q.Get("error_description"); desc != "" {
		return fmt.Errorf("xal/sisu: login rejected: %s: %s", code, desc)
	}
	return fmt.Errorf("xal/sisu: login rejected: %s", code)
}

// loginError inspects the HTML returned when the login POST did not yield an
// authorization code and returns the clearest possible explanation. Where the
// page embeds a ServerData error (typically wrong credentials), that message is
// surfaced; an interrupt/consent page (which the headless flow cannot satisfy)
// is reported as such; otherwise a generic fragility error is returned.
func loginError(html string) error {
	if data, err := extractServerData(html); err == nil && data.ErrorText != "" {
		if data.ErrorCode != "" {
			return fmt.Errorf("xal/sisu: login failed: %s (%s)", data.ErrorText, data.ErrorCode)
		}
		return fmt.Errorf("xal/sisu: login failed: %s", data.ErrorText)
	}
	if strings.Contains(html, `<body onload="javascript:DoSubmit();">`) {
		return errors.New("xal/sisu: login requires an interactive interrupt (2FA, consent, or conditional access); use the device-code or authorization-code flow instead")
	}
	return errors.New("xal/sisu: login did not produce an authorization code; the account likely requires 2FA, a CAPTCHA, or conditional access, which the headless credentials flow cannot satisfy")
}
