// Package proxy — share.go is the HTTP-backed ShareValidator. It calls
// vajra-master's `/internal/proxy/validate-share` endpoint to confirm a
// token is alive, scoped to the given sandbox, and (optionally) for the
// requested port.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// HTTPShareValidator implements ShareValidator by hitting master.
// BaseURL is the master URL prefix; Token is the shared internal secret
// included as a Bearer token. Client may be nil — http.DefaultClient is
// used.
type HTTPShareValidator struct {
	BaseURL string
	Token   string
	Client  *http.Client
}

// ValidateShare returns nil if master accepts the (sandboxID, token,
// port) tuple. Any non-2xx response or transport error becomes a
// non-nil error so the proxy can deny the request.
func (v *HTTPShareValidator) ValidateShare(ctx context.Context, sandboxID, token string, port int) error {
	c := v.Client
	if c == nil {
		c = http.DefaultClient
	}
	q := url.Values{}
	q.Set("sandbox_id", sandboxID)
	q.Set("token", token)
	if port > 0 {
		q.Set("port", strconv.Itoa(port))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		v.BaseURL+"/internal/proxy/validate-share?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	if v.Token != "" {
		req.Header.Set("Authorization", "Bearer "+v.Token)
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusNotFound, http.StatusUnauthorized, http.StatusForbidden, http.StatusGone:
		return errors.New("share token rejected")
	default:
		return fmt.Errorf("master share check responded %d", resp.StatusCode)
	}
}
