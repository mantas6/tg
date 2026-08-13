// Package api is a thin client for the Toggl Track API v9. It uses HTTP Basic
// auth (the API token as username, the literal "api_token" as password) and
// maps non-2xx responses to Go errors. The base URL and *http.Client are
// injectable so tests can point at an httptest.Server.
//
// Every call takes a context.Context: it is what makes a hung request (or a
// Ctrl-C, see the ctx main wires up) abort instead of blocking for the client's
// whole timeout, and it also bounds the rate-limit backoff (see doURL).
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is the production Toggl Track API v9 root.
const DefaultBaseURL = "https://api.track.toggl.com/api/v9"

// DefaultReportsBaseURL is the production Toggl Reports API v3 root. It lives on
// a different path prefix than the v9 API, so it is tracked separately (see
// SummaryByTask).
const DefaultReportsBaseURL = "https://api.track.toggl.com/reports/api/v3"

// ErrUnauthorized is returned when Toggl rejects the credentials (HTTP 401):
// the token is missing, malformed or revoked, so no request will work until
// `tg auth` stores a new one.
var ErrUnauthorized = errors.New("invalid API token")

// ErrForbidden is returned when Toggl accepts the credentials but refuses the
// request (HTTP 403), e.g. a workspace the token has no access to or a
// plan-gated endpoint. It is kept apart from ErrUnauthorized because the
// remedies differ: re-authenticating cannot fix a 403.
var ErrForbidden = errors.New("access forbidden")

// defaultAttempts is how many times a rate-limited (HTTP 429) request is sent
// before its error is surfaced: the original plus two retries. Toggl's limiter
// is short-lived, so a couple of attempts covers it without turning a genuine
// outage into a long stall.
const defaultAttempts = 3

// defaultBackoff is the pause between attempts when the server does not say how
// long to wait (see retryWait).
const defaultBackoff = 500 * time.Millisecond

// maxBackoff caps whatever a Retry-After header asks for, so a server (or a
// proxy) naming a very long delay cannot wedge the CLI for minutes.
const maxBackoff = 10 * time.Second

// Client talks to the Toggl Track API.
type Client struct {
	token          string
	baseURL        string
	reportsBaseURL string
	httpClient     *http.Client
	attempts       int
	backoff        time.Duration
}

// Option configures a Client.
type Option func(*Client)

// WithBaseURL overrides the API v9 root (used in tests).
func WithBaseURL(u string) Option {
	return func(c *Client) { c.baseURL = strings.TrimRight(u, "/") }
}

// WithReportsBaseURL overrides the Reports API v3 root (used in tests).
func WithReportsBaseURL(u string) Option {
	return func(c *Client) { c.reportsBaseURL = strings.TrimRight(u, "/") }
}

// WithHTTPClient injects a custom *http.Client (used in tests).
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.httpClient = h }
}

// WithRetry overrides the rate-limit retry policy: attempts is the total number
// of tries per request (values below 1 mean 1, i.e. no retry) and backoff is the
// pause used when the response carries no Retry-After. Tests use it to keep the
// retry path fast.
func WithRetry(attempts int, backoff time.Duration) Option {
	return func(c *Client) {
		if attempts < 1 {
			attempts = 1
		}
		c.attempts = attempts
		c.backoff = backoff
	}
}

// New returns a Client authenticating with the given API token.
func New(token string, opts ...Option) *Client {
	c := &Client{
		token:          token,
		baseURL:        DefaultBaseURL,
		reportsBaseURL: DefaultReportsBaseURL,
		httpClient:     &http.Client{Timeout: 30 * time.Second},
		attempts:       defaultAttempts,
		backoff:        defaultBackoff,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// do performs an HTTP request against the v9 API (c.baseURL).
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	return c.doURL(ctx, method, c.baseURL+path, body, out)
}

// doReports performs an HTTP request against the Reports API v3
// (c.reportsBaseURL), which lives on a different path prefix than the v9 API.
func (c *Client) doReports(ctx context.Context, method, path string, body, out any) error {
	return c.doURL(ctx, method, c.reportsBaseURL+path, body, out)
}

// doURL performs an HTTP request to a fully-qualified url, marshaling body (if
// non-nil) as JSON and unmarshaling a 2xx response into out (if non-nil).
// Non-2xx responses become errors: 401 -> ErrUnauthorized, 403 -> ErrForbidden,
// otherwise an error carrying the status and (possibly non-JSON) response body.
//
// A rate-limited response (429) is retried up to c.attempts times, waiting what
// Retry-After asks for or c.backoff otherwise (see retryWait). The wait is
// interruptible: a cancelled ctx aborts it and returns the context's error
// rather than sleeping on. The request body is marshaled once and re-read per
// attempt, so retries send the same bytes.
//
// ONLY 429 is retried, and deliberately so: a limiter rejects a request before
// acting on it, so re-sending even a POST cannot duplicate an entry. Other
// failures (a 5xx, a dropped connection) may have been applied server-side, so
// they are surfaced instead — the entry stays dirty and the next `tg push`
// reconciles it under LWW.
func (c *Client) doURL(ctx context.Context, method, url string, body, out any) error {
	var payload []byte
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = buf
	}

	attempts := c.attempts
	if attempts < 1 {
		attempts = 1
	}
	for attempt := 1; ; attempt++ {
		status, respBody, header, err := c.send(ctx, method, url, payload)
		if err != nil {
			return err
		}
		if status == http.StatusTooManyRequests && attempt < attempts {
			if err := sleep(ctx, retryWait(header, c.backoff)); err != nil {
				return err
			}
			continue
		}
		return decode(status, respBody, out)
	}
}

// send performs one attempt and returns the response status, body and headers.
func (c *Client) send(ctx context.Context, method, url string, payload []byte) (int, []byte, http.Header, error) {
	var reqBody io.Reader
	if payload != nil {
		reqBody = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return 0, nil, nil, err
	}
	req.SetBasicAuth(c.token, "api_token")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, nil, err
	}
	return resp.StatusCode, respBody, resp.Header, nil
}

// decode turns one response into the result of a call: an error for a non-2xx
// status (carrying the server's own message, which is what makes a rejected
// entry diagnosable) or the decoded body in out.
func decode(status int, respBody []byte, out any) error {
	msg := strings.TrimSpace(string(respBody))
	switch {
	case status == http.StatusUnauthorized:
		return withBody(ErrUnauthorized, msg)
	case status == http.StatusForbidden:
		return withBody(ErrForbidden, msg)
	case status < 200 || status >= 300:
		if msg == "" {
			msg = http.StatusText(status)
		}
		return fmt.Errorf("toggl api: status %d: %s", status, msg)
	}

	if out == nil || len(bytes.TrimSpace(respBody)) == 0 {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	return nil
}

// withBody appends the server's response text to a sentinel error, keeping
// errors.Is working while still reporting what Toggl actually said.
func withBody(err error, msg string) error {
	if msg == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, msg)
}

// retryWait decides how long to wait before re-sending a rate-limited request:
// what the server's Retry-After header asks for (delay-seconds only, capped at
// maxBackoff), else the client's fixed backoff.
func retryWait(header http.Header, backoff time.Duration) time.Duration {
	if header != nil {
		if v := strings.TrimSpace(header.Get("Retry-After")); v != "" {
			if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
				d := time.Duration(secs) * time.Second
				if d > maxBackoff {
					return maxBackoff
				}
				return d
			}
		}
	}
	return backoff
}

// sleep waits for d, aborting early (with the context's error) if ctx is
// cancelled. A non-positive d only checks whether ctx is already done.
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
