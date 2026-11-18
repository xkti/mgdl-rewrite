package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"
)

// defaultAPI is MEGA's command server. It is always contacted directly over the
// user's own connection; only the bulk file transfer goes through the proxy.
const defaultAPI = "https://g.api.mega.co.nz/cs"

// apiError is a negative error code returned by the MEGA API.
// See https://github.com/meganz/sdk/blob/master/include/mega/types.h (error enum).
type apiError int

func (e apiError) Error() string {
	if msg, ok := apiErrorText[int(e)]; ok {
		return fmt.Sprintf("MEGA API error %d (%s)", int(e), msg)
	}
	return fmt.Sprintf("MEGA API error %d", int(e))
}

var apiErrorText = map[int]string{
	-1:  "internal error",
	-2:  "bad arguments",
	-3:  "try again",
	-4:  "rate limited",
	-6:  "too many requests",
	-8:  "link expired",
	-9:  "not found — the link is wrong or was removed",
	-11: "access denied",
	-14: "decryption failed",
	-16: "blocked — taken down or the account was suspended",
	-17: "over quota — the owner's account has exceeded its storage quota",
	-18: "temporarily unavailable",
}

// permanent reports whether retrying the same request can't succeed (soon enough
// to be worth waiting for: -17 means the owner's account is over quota).
func (e apiError) permanent() bool {
	switch int(e) {
	case -2, -8, -9, -11, -14, -16, -17:
		return true
	}
	return false
}

// throttled reports whether the API is asking us to slow down. Like HTTP 429/503/509
// from storage, these are waited out without spending a retry.
func (e apiError) throttled() bool {
	return e == -4 || e == -6
}

// apiThrottleWait is the base pause after the API says we are going too fast; each
// consecutive throttle waits one step longer, up to apiThrottleMax. A variable so
// tests can shrink it.
var apiThrottleWait = 5 * time.Second

const apiThrottleMax = 30 * time.Second

// throttleNoteKey carries a callback that call tells about each throttle wait,
// so whoever started the call can say so (see withThrottleNote).
type throttleNoteKey struct{}

// withThrottleNote makes API calls under ctx report their throttle waits to fn.
func withThrottleNote(ctx context.Context, fn func(code int, wait time.Duration)) context.Context {
	return context.WithValue(ctx, throttleNoteKey{}, fn)
}

// throttleDelay is the pause before retrying after the nth consecutive throttle.
func throttleDelay(n int) time.Duration {
	return min(time.Duration(n)*apiThrottleWait, apiThrottleMax)
}

// apiClient issues MEGA API commands.
type apiClient struct {
	http     *http.Client
	endpoint string
	seq      atomic.Int64
	retries  int // attempts for transient failures (0 = forever)
}

func newAPIClient(hc *http.Client, retries int) *apiClient {
	c := &apiClient{http: hc, endpoint: defaultAPI, retries: retries}
	c.seq.Store(rand.Int63n(1 << 30))
	return c
}

// call sends one command (as a single-element batch) and decodes the single result
// into out. folder, if non-empty, is the public folder handle the command runs in.
//
// Permanent errors return at once. Throttling (-4/-6) is waited out without spending
// a retry, up to maxThrottleWaits times. Anything else transient (-3, -18, HTTP 5xx,
// network errors) is retried with backoff, up to c.retries attempts (0 = forever).
func (c *apiClient) call(ctx context.Context, folder string, cmd any, out any) error {
	var lastErr error
	throttles := 0
	for attempt := 1; c.retries == 0 || attempt <= c.retries; {
		err := c.callOnce(ctx, folder, cmd, out)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var ae apiError
		if errors.As(err, &ae) && ae.permanent() {
			return err
		}
		lastErr = err

		var wait time.Duration
		if errors.As(err, &ae) && ae.throttled() && throttles < maxThrottleWaits {
			throttles++
			wait = throttleDelay(throttles)
			debugf("api: %v; waiting %s (throttled %d/%d, not counted as a retry)", err, wait, throttles, maxThrottleWaits)
			if fn, ok := ctx.Value(throttleNoteKey{}).(func(int, time.Duration)); ok {
				fn(int(ae), wait)
			}
		} else {
			wait = min(time.Duration(attempt*attempt)*500*time.Millisecond, 10*time.Second)
			debugf("api: %v; retrying in %s (attempt %d)", err, wait, attempt)
			attempt++
		}
		if !sleepCtx(ctx, wait) {
			return ctx.Err()
		}
	}
	return lastErr
}

// callOnce makes a single attempt with no retries, for callers that run their own
// retry loop (the chunk downloader), so retries don't multiply.
func (c *apiClient) callOnce(ctx context.Context, folder string, cmd any, out any) error {
	body, err := json.Marshal([]any{cmd})
	if err != nil {
		return err
	}
	raw, err := c.post(ctx, folder, body)
	if err != nil {
		return err
	}
	return decodeAPIResult(raw, out)
}

func (c *apiClient) post(ctx context.Context, folder string, body []byte) ([]byte, error) {
	u := c.endpoint + "?id=" + strconv.FormatInt(c.seq.Add(1), 10)
	if folder != "" {
		u += "&n=" + folder
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	debugf("api: POST %s %s", u, truncateBytes(body, 200))
	start := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("api request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return nil, fmt.Errorf("api response: %w", err)
	}
	debugf("api: HTTP %d, %d bytes in %s: %s", resp.StatusCode, len(raw),
		time.Since(start).Round(time.Millisecond), truncateBytes(raw, 200))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("api: HTTP %s", resp.Status)
	}
	return raw, nil
}

// decodeAPIResult unpacks a response to a single-command batch. MEGA answers with a
// bare number for request-level errors, or an array holding either an object or a
// number per command.
func decodeAPIResult(raw []byte, out any) error {
	raw = bytes.TrimSpace(raw)
	if n, err := strconv.Atoi(string(raw)); err == nil {
		if n < 0 {
			return apiError(n)
		}
		return fmt.Errorf("api: unexpected response %q", raw)
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil || len(arr) != 1 {
		return fmt.Errorf("api: malformed response %q", truncateBytes(raw, 100))
	}
	el := bytes.TrimSpace(arr[0])
	if n, err := strconv.Atoi(string(el)); err == nil {
		if n < 0 {
			return apiError(n)
		}
	}
	// Some commands report errors inside the object as {"e": -N}.
	var probe struct {
		E *int `json:"e"`
	}
	if json.Unmarshal(el, &probe) == nil && probe.E != nil && *probe.E < 0 {
		return apiError(*probe.E)
	}
	return json.Unmarshal(el, out)
}

// fileInfo is the answer to an "a":"g" command.
type fileInfo struct {
	Size int64  `json:"s"`
	Attr string `json:"at"`
	URL  string `json:"g"`
}

// getCmd builds an "a":"g" command. For a file inside a folder share, folder is
// the share handle and handle the node handle; for a public file link, folder is ""
// and handle is the link handle.
func getCmd(folder, handle string, wantURL bool) map[string]any {
	cmd := map[string]any{"a": "g"}
	if folder == "" {
		cmd["p"] = handle
	} else {
		cmd["n"] = handle
	}
	if wantURL {
		cmd["g"] = 1
	}
	return cmd
}

// getFile asks for a file's size and attributes, retrying as call does.
func (c *apiClient) getFile(ctx context.Context, folder, handle string) (*fileInfo, error) {
	var fi fileInfo
	if err := c.call(ctx, folder, getCmd(folder, handle, false), &fi); err != nil {
		return nil, err
	}
	return &fi, nil
}

// getLink asks once for a fresh download URL. Download URLs are short-lived: request
// a new one for every chunk attempt. There are no retries here; the chunk's own retry
// loop decides what to do with a failure.
func (c *apiClient) getLink(ctx context.Context, folder, handle string) (*fileInfo, error) {
	var fi fileInfo
	if err := c.callOnce(ctx, folder, getCmd(folder, handle, true), &fi); err != nil {
		return nil, err
	}
	if fi.URL == "" {
		return nil, errors.New("api returned no download URL")
	}
	return &fi, nil
}

// rawNode is one entry of a folder listing.
type rawNode struct {
	Handle string `json:"h"`
	Parent string `json:"p"`
	Type   int    `json:"t"` // 0 file, 1 folder
	Attr   string `json:"a"`
	Key    string `json:"k"` // "h1:key1/h2:key2…"
	Size   int64  `json:"s"`
	TS     int64  `json:"ts"`
}

// listFolder fetches every node in a public folder share.
func (c *apiClient) listFolder(ctx context.Context, folder string) ([]rawNode, error) {
	var res struct {
		F []rawNode `json:"f"`
	}
	cmd := map[string]any{"a": "f", "c": 1, "ca": 1, "r": 1}
	if err := c.call(ctx, folder, cmd, &res); err != nil {
		return nil, err
	}
	return res.F, nil
}

func truncateBytes(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
