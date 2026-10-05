package marketmaker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// APIError is a non-success response from the API.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%d %s: %s", e.Status, e.Code, e.Message)
}

// HTTPClient talks to the public API as one market maker user. It logs in
// lazily, re-logs in when its token expires, and polls prices with ETags so
// unchanged prices cost a 304.
type HTTPClient struct {
	BaseURL            string
	Username, Password string
	HTTP               *http.Client

	mu    sync.Mutex
	token string
	etag  string
	snap  Snapshot
}

func NewHTTPClient(baseURL, username, password string) *HTTPClient {
	return &HTTPClient{
		BaseURL: strings.TrimRight(baseURL, "/"), Username: username, Password: password,
		HTTP: &http.Client{Timeout: 5 * time.Second},
	}
}

func (c *HTTPClient) Tickers(ctx context.Context) ([]Ticker, error) {
	var out struct {
		Symbols []Ticker `json:"symbols"`
	}
	_, err := c.do(ctx, "GET", "/v1/market/symbols", nil, &out, false)
	return out.Symbols, err
}

func (c *HTTPClient) Prices(ctx context.Context) (Snapshot, error) {
	c.mu.Lock()
	etag, cached := c.etag, c.snap
	c.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, "GET", c.BaseURL+"/v1/market/prices", nil)
	if err != nil {
		return Snapshot{}, err
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return Snapshot{}, err
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusNotModified:
		io.Copy(io.Discard, res.Body)
		return cached, nil
	case http.StatusOK:
		var snap Snapshot
		if err := json.NewDecoder(res.Body).Decode(&snap); err != nil {
			return Snapshot{}, err
		}
		c.mu.Lock()
		c.etag, c.snap = res.Header.Get("ETag"), snap
		c.mu.Unlock()
		return snap, nil
	default:
		return Snapshot{}, decodeError(res)
	}
}

func (c *HTTPClient) Account(ctx context.Context) (Account, error) {
	var a Account
	_, err := c.do(ctx, "GET", "/v1/account", nil, &a, true)
	return a, err
}

func (c *HTTPClient) PlaceOrder(ctx context.Context, o Order) error {
	_, err := c.do(ctx, "POST", "/v1/orders", o, nil, true)
	return err
}

func (c *HTTPClient) login(ctx context.Context) error {
	var out struct {
		Token string `json:"token"`
	}
	creds := map[string]string{"username": c.Username, "password": c.Password}
	if _, err := c.do(ctx, "POST", "/v1/auth/login", creds, &out, false); err != nil {
		return fmt.Errorf("logging in as %s: %w", c.Username, err)
	}
	c.mu.Lock()
	c.token = out.Token
	c.mu.Unlock()
	return nil
}

// do sends a JSON request, logging in first (and once more on a 401) when
// authenticated.
func (c *HTTPClient) do(ctx context.Context, method, path string, body, out any, authed bool) (int, error) {
	for attempt := 0; ; attempt++ {
		c.mu.Lock()
		token := c.token
		c.mu.Unlock()
		if authed && token == "" {
			if err := c.login(ctx); err != nil {
				return 0, err
			}
			continue
		}

		var rd io.Reader
		if body != nil {
			b, err := json.Marshal(body)
			if err != nil {
				return 0, err
			}
			rd = bytes.NewReader(b)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rd)
		if err != nil {
			return 0, err
		}
		req.Header.Set("Content-Type", "application/json")
		if authed {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := c.HTTP.Do(req)
		if err != nil {
			return 0, err
		}
		if res.StatusCode == http.StatusUnauthorized && authed && attempt == 0 {
			res.Body.Close()
			c.mu.Lock()
			c.token = ""
			c.mu.Unlock()
			continue
		}
		defer res.Body.Close()
		if res.StatusCode >= 300 {
			return res.StatusCode, decodeError(res)
		}
		if out != nil {
			return res.StatusCode, json.NewDecoder(res.Body).Decode(out)
		}
		io.Copy(io.Discard, res.Body)
		return res.StatusCode, nil
	}
}

func decodeError(res *http.Response) error {
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	json.NewDecoder(res.Body).Decode(&body)
	return &APIError{Status: res.StatusCode, Code: body.Error.Code, Message: body.Error.Message}
}
