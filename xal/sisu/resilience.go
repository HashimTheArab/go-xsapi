package sisu

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// clusterAffinityHeader is the response header carrying the SISU cluster host
// that follow-up requests should be routed to.
//
// ASSUMPTION: the exact header name is not recovered from the binary (we only
// have the debug strings "Cluster Affinity header ... from SISU signin
// response"). The real client refers to it as "Cluster Affinity"; the most
// likely wire spelling is the canonicalised "X-Cluster-Affinity" used by other
// Xbox Live headers (compare "X-Err", "X-SessionId"). Confirming this requires
// a live SISU traffic capture (mitmproxy on a real device). The value is read
// through this single const so the spelling can be corrected in one place.
const clusterAffinityHeader = "X-Cluster-Affinity"

// sessionIDHeader is the response header carrying the SISU session ID.
//
// The existing account-creation path already reads "X-SessionId" (see
// accountCreationRequired); this const documents the same header for the
// signin-response path the binary references ("Session ID header ... from SISU
// signin response").
const sessionIDHeader = "X-SessionId"

// clusterAffinity reads and validates the Cluster Affinity header from a SISU
// signin response.
//
// The real Minecraft client routes follow-up SISU requests to the cluster host
// advertised in this header. Behaviour, per recovered debug strings:
//   - header missing  -> (nil, nil): not fatal; the caller falls back to the
//     default SISU host ("Cluster Affinity header missing from SISU signin
//     response.").
//   - value not a URL -> error ("Cluster Affinity header is invalid from SISU
//     signin response.").
//   - value not https -> error ("Cluster Affinity header is not https from SISU
//     signin response.").
func clusterAffinity(header http.Header) (*url.URL, error) {
	value := header.Get(clusterAffinityHeader)
	if value == "" {
		// Absence is handled, not fatal: fall back to the default host.
		return nil, nil
	}
	u, err := url.Parse(value)
	if err != nil {
		return nil, fmt.Errorf("xal/sisu: Cluster Affinity header is invalid from SISU signin response: %w", err)
	}
	if u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("xal/sisu: Cluster Affinity header is not https from SISU signin response: %q", value)
	}
	return u, nil
}

// sessionID reads the Session ID header from a SISU signin response. An empty
// string indicates the header is absent ("Session ID header missing from SISU
// signin response.").
func sessionID(header http.Header) string {
	return header.Get(sessionIDHeader)
}

// errorCodeSPOP is the X-Err code reported when SISU rejects a sign-in because
// the title is already signed in elsewhere (Single Point Of Presence). The real
// client handles this by signing in here and retrying the token fetch
// ("SpopSignInHere succeeded. Retrying GetSisuTokens").
//
// ASSUMPTION: the numeric SPOP X-Err code is not recovered from the binary. We
// only have the debug strings, not the constant. The documented Xbox HRESULT
// closest in meaning is ErrorCodeTitleSinglePointOfPresenceViolated
// (0x8015DC1E); we reuse it as the SPOP code rather than inventing a new value.
// Confirming the real wire value requires a live SISU error capture. Retry
// eligibility is centralised in isRetryableXErr so the set can be adjusted in
// one place once real codes are observed.
const errorCodeSPOP = ErrorCodeTitleSinglePointOfPresenceViolated

// isRetryableXErr reports whether a SISU X-Err code denotes a transient,
// recoverable condition that warrants a single retry of the token fetch rather
// than failing outright.
//
// The real client retries after handling SPOP ("Retrying GetSisuTokens"). This
// predicate is the extension point for that decision: add codes here as their
// real wire values are confirmed (for example a Curfew X-Err, which the binary
// references but whose numeric value is likewise unknown).
func isRetryableXErr(code ErrorCode) bool {
	switch code {
	case errorCodeSPOP:
		return true
	default:
		return false
	}
}

// retryableResponse reports whether resp is a SISU error response whose X-Err
// code is retryable per isRetryableXErr. Responses without a parseable
// retryable X-Err return false so the caller fails (or succeeds) normally.
func retryableResponse(resp *http.Response) bool {
	if resp == nil || resp.StatusCode == http.StatusOK {
		return false
	}
	xerr := resp.Header.Get("X-Err")
	if xerr == "" {
		return false
	}
	n, err := strconv.ParseUint(xerr, 10, 32)
	if err != nil {
		return false
	}
	return isRetryableXErr(ErrorCode(n))
}

// authorizeWithRetry performs a SISU authorize attempt and, when the response
// carries a retryable X-Err code, retries exactly once.
//
// The retry is bounded to a single additional attempt — matching the real
// client's "Retrying GetSisuTokens" (a single retry, not a loop) — and re-entry
// is prevented by the fixed loop bound. The returned response is left unread so
// the caller can decode the body (on success) or build the rich error (on a
// terminal failure). attempt is responsible for sending the request; a non-nil
// transport error from attempt is returned immediately without retrying.
func (s *Session) authorizeWithRetry(ctx context.Context, attempt func(context.Context) (*http.Response, error)) (*http.Response, error) {
	const maxAttempts = 2 // initial attempt + one retry

	var resp *http.Response
	for i := 0; i < maxAttempts; i++ {
		var err error
		resp, err = attempt(ctx)
		if err != nil {
			return nil, err
		}
		last := i == maxAttempts-1
		if last || !retryableResponse(resp) {
			return resp, nil
		}
		// Discard the retryable error response body before the next attempt so
		// the underlying connection can be reused.
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
	}
	return resp, nil
}
