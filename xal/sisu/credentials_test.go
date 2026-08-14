package sisu

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// A realistic ServerData snippet as served by login.live.com's authorize page.
// The sFTTag value uses JSON-escaped quotes, exactly like the real page.
const serverDataHTML = `<!DOCTYPE html><html><head></head><body>
<script type="text/javascript">
//<![CDATA[
var ServerData = {"sFTName":"PPFT","sFTTag":"<input type=\"hidden\" name=\"PPFT\" id=\"i0327\" value=\"abcPPFTtoken123\"/>","urlPost":"https://login.live.com/ppsecure/post.srf?contextid=ABC123&opid=DEF","iMaxStackForKMSI":15};
//]]>
</script>
</body></html>`

func TestExtractServerData(t *testing.T) {
	data, err := extractServerData(serverDataHTML)
	if err != nil {
		t.Fatalf("extractServerData: %v", err)
	}
	if want := "https://login.live.com/ppsecure/post.srf?contextid=ABC123&opid=DEF"; data.URLPost != want {
		t.Fatalf("urlPost = %q, want %q", data.URLPost, want)
	}
	if !strings.Contains(data.SFTTag, `value="abcPPFTtoken123"`) {
		t.Fatalf("sFTTag = %q, want it to contain the PPFT value", data.SFTTag)
	}
}

func TestExtractServerDataErrors(t *testing.T) {
	if _, err := extractServerData("<html>no server data here</html>"); err == nil {
		t.Fatal("extractServerData succeeded, want error for missing marker")
	}
	if _, err := extractServerData("var ServerData = not-json;"); err == nil {
		t.Fatal("extractServerData succeeded, want error for invalid JSON")
	}
}

func TestParsePPFT(t *testing.T) {
	data, err := extractServerData(serverDataHTML)
	if err != nil {
		t.Fatalf("extractServerData: %v", err)
	}
	name, value, err := parsePPFT(data.SFTTag)
	if err != nil {
		t.Fatalf("parsePPFT: %v", err)
	}
	if name != "PPFT" {
		t.Fatalf("name = %q, want PPFT", name)
	}
	if value != "abcPPFTtoken123" {
		t.Fatalf("value = %q, want abcPPFTtoken123", value)
	}
}

func TestParsePPFTErrors(t *testing.T) {
	tests := []struct {
		name   string
		sFTTag string
	}{
		{"no value", `<input type="hidden" name="PPFT"/>`},
		{"no name", `<input type="hidden" value="tok"/>`},
		{"empty", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := parsePPFT(tt.sFTTag); err == nil {
				t.Fatal("parsePPFT succeeded, want error")
			}
		})
	}
}

func TestCodeFromURL(t *testing.T) {
	tests := []struct {
		name   string
		rawURL string
		want   string
		wantOK bool
	}{
		{
			name:   "desktop redirect with code",
			rawURL: "https://login.live.com/oauth20_desktop.srf?code=M.C123_BAY.2.deadbeef&lc=1033",
			want:   "M.C123_BAY.2.deadbeef",
			wantOK: true,
		},
		{"no code", "https://login.live.com/oauth20_desktop.srf?lc=1033", "", false},
		{"empty code", "https://login.live.com/oauth20_desktop.srf?code=", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := url.Parse(tt.rawURL)
			if err != nil {
				t.Fatalf("parse URL: %v", err)
			}
			got, ok := codeFromURL(u)
			if got != tt.want || ok != tt.wantOK {
				t.Fatalf("codeFromURL = (%q, %v), want (%q, %v)", got, ok, tt.want, tt.wantOK)
			}
		})
	}
	if _, ok := codeFromURL(nil); ok {
		t.Fatal("codeFromURL(nil) ok = true, want false")
	}
}

func TestRedirectError(t *testing.T) {
	u, err := url.Parse("https://login.live.com/oauth20_desktop.srf?error=invalid_scope&error_description=The+scope+is+invalid")
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}
	got := redirectError(u)
	if got == nil {
		t.Fatal("redirectError = nil, want error")
	}
	if !strings.Contains(got.Error(), "invalid_scope") || !strings.Contains(got.Error(), "The scope is invalid") {
		t.Fatalf("redirectError = %q, want it to mention the error and description", got)
	}

	clean, err := url.Parse("https://login.live.com/oauth20_desktop.srf?code=abc")
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}
	if err := redirectError(clean); err != nil {
		t.Fatalf("redirectError = %v, want nil for a URL without error", err)
	}
	if err := redirectError(nil); err != nil {
		t.Fatalf("redirectError(nil) = %v, want nil", err)
	}
}

func TestLoginError(t *testing.T) {
	incorrect := `<html><body><script>var ServerData = {"sErrorCode":"1041","sErrTxt":"Your account or password is incorrect."};</script></body></html>`
	if err := loginError(incorrect); err == nil || !strings.Contains(err.Error(), "Your account or password is incorrect.") || !strings.Contains(err.Error(), "1041") {
		t.Fatalf("loginError(incorrect) = %v, want it to surface the ServerData error", err)
	}

	interrupt := `<html><body onload="javascript:DoSubmit();"><form action="https://login.live.com/ppsecure/post.srf?ru=x"></form></body></html>`
	if err := loginError(interrupt); err == nil || !strings.Contains(err.Error(), "interactive interrupt") {
		t.Fatalf("loginError(interrupt) = %v, want an interrupt error", err)
	}

	generic := `<html><body>something unexpected</body></html>`
	if err := loginError(generic); err == nil || !strings.Contains(err.Error(), "did not produce an authorization code") {
		t.Fatalf("loginError(generic) = %v, want a generic error", err)
	}
}

// setCredentialsEndpoints redirects the credentials flow at a local server for
// the duration of a test and returns a restore func.
func setCredentialsEndpoints(authorize, tokenURL string) func() {
	prevAuthorize, prevToken := credentialsAuthorizeURL, credentialsTokenURL
	credentialsAuthorizeURL = authorize
	credentialsTokenURL = tokenURL
	return func() {
		credentialsAuthorizeURL = prevAuthorize
		credentialsTokenURL = prevToken
	}
}

func TestCredentialsTokenEndToEnd(t *testing.T) {
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth20_authorize.srf", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if got := q.Get("response_type"); got != "code" {
			t.Errorf("response_type = %q, want code", got)
		}
		if got := q.Get("response_mode"); got != "query" {
			t.Errorf("response_mode = %q, want query", got)
		}
		if got := q.Get("client_id"); got != "client-id" {
			t.Errorf("client_id = %q, want client-id", got)
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<html><body><script>var ServerData = {"sFTTag":"<input type=\"hidden\" name=\"PPFT\" value=\"tok123\"/>","urlPost":%q};</script></body></html>`, srv.URL+"/login")
	})
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse login form: %v", err)
		}
		for field, want := range map[string]string{
			"login":    "user@example.com",
			"loginfmt": "user@example.com",
			"passwd":   "hunter2",
			"PPFT":     "tok123",
		} {
			if got := r.PostForm.Get(field); got != want {
				t.Errorf("login form %q = %q, want %q", field, got, want)
			}
		}
		http.Redirect(w, r, srv.URL+"/oauth20_desktop.srf?code=AUTHCODE", http.StatusFound)
	})
	mux.HandleFunc("/oauth20_desktop.srf", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})
	mux.HandleFunc("/oauth20_token.srf", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse token form: %v", err)
		}
		if got := r.PostForm.Get("grant_type"); got != "authorization_code" {
			t.Errorf("grant_type = %q, want authorization_code", got)
		}
		if got := r.PostForm.Get("code"); got != "AUTHCODE" {
			t.Errorf("code = %q, want AUTHCODE", got)
		}
		if got := r.PostForm.Get("redirect_uri"); got != credentialsRedirectURI {
			t.Errorf("redirect_uri = %q, want %q", got, credentialsRedirectURI)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"ACCESS","token_type":"bearer","refresh_token":"REFRESH","expires_in":3600}`)
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	defer setCredentialsEndpoints(srv.URL+"/oauth20_authorize.srf", srv.URL+"/oauth20_token.srf")()

	token, err := (Config{ClientID: "client-id"}).CredentialsToken(context.Background(), "user@example.com", "hunter2")
	if err != nil {
		t.Fatalf("CredentialsToken: %v", err)
	}
	if token.AccessToken != "ACCESS" {
		t.Fatalf("access token = %q, want ACCESS", token.AccessToken)
	}
	if token.RefreshToken != "REFRESH" {
		t.Fatalf("refresh token = %q, want REFRESH", token.RefreshToken)
	}
}

func TestCredentialsTokenLoginError(t *testing.T) {
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth20_authorize.srf", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<html><body><script>var ServerData = {"sFTTag":"<input name=\"PPFT\" value=\"tok\"/>","urlPost":%q};</script></body></html>`, srv.URL+"/login")
	})
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, `<html><body><script>var ServerData = {"sErrorCode":"1041","sErrTxt":"Your account or password is incorrect."};</script></body></html>`)
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	defer setCredentialsEndpoints(srv.URL+"/oauth20_authorize.srf", srv.URL+"/oauth20_token.srf")()

	_, err := (Config{ClientID: "client-id"}).CredentialsToken(context.Background(), "user@example.com", "wrong")
	if err == nil {
		t.Fatal("CredentialsToken succeeded, want error")
	}
	if !strings.Contains(err.Error(), "password is incorrect") {
		t.Fatalf("error = %q, want it to mention the incorrect password", err)
	}
}
