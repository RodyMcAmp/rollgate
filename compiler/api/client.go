package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"
)

// Socket is where rollgate-api.socket listens.
const Socket = "/run/rollgate/api.sock"

// Client talks to rollgate api over its Unix socket; the web server uses it.
type Client struct {
	http *http.Client
}

// NewClient returns a client for the API socket at path.
func NewClient(path string) *Client {
	return &Client{http: &http.Client{
		// Updates take minutes; status and health a few seconds.
		Timeout: 30 * time.Minute,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", path)
			},
		},
	}}
}

// Get asks a read request, such as "/v1/status".
func (c *Client) Get(ctx context.Context, path string) (Response, error) {
	return c.do(ctx, http.MethodGet, path, nil)
}

// Post asks a change, such as "/v1/apply".
func (c *Client) Post(ctx context.Context, path string, req Request) (Response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return Response{}, err
	}
	return c.do(ctx, http.MethodPost, path, body)
}

// do returns the API's answer. A refusal (OK false) is not an error; not
// reaching the API, or an answer that isn't the API's, is.
func (c *Client) do(ctx context.Context, method, path string, body []byte) (Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://rollgate"+path, bytes.NewReader(body))
	if err != nil {
		return Response{}, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return Response{}, fmt.Errorf("the router's API didn't answer: %w", err)
	}
	defer resp.Body.Close()
	var r Response
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return Response{}, fmt.Errorf("the router's API answered %s, not a result", resp.Status)
	}
	return r, nil
}

// SystemdListeners returns the sockets systemd passed to this process (socket
// activation), or an error if there are none.
func SystemdListeners() ([]net.Listener, error) {
	n, err := strconv.Atoi(os.Getenv("LISTEN_FDS"))
	if os.Getenv("LISTEN_PID") != strconv.Itoa(os.Getpid()) || err != nil || n < 1 {
		return nil, errors.New("no sockets from systemd")
	}
	var ls []net.Listener
	for fd := 3; fd < 3+n; fd++ { // SD_LISTEN_FDS_START
		f := os.NewFile(uintptr(fd), "systemd socket")
		l, err := net.FileListener(f)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("socket %d from systemd: %w", fd, err)
		}
		ls = append(ls, l)
	}
	return ls, nil
}
