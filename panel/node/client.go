package node

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"
)

// Client drives a remote node over mTLS. It IS the remote Runner: there is no
// separate wrapper type, because a wrapper would only forward six methods
// unchanged and give the conformance suite a second place for the two paths to
// diverge.
type Client struct {
	base string
	http *http.Client
}

var _ Runner = (*Client)(nil)

type ClientConfig struct {
	Address string // host or IP the node's certificate was issued for
	Port    int
	CACert  []byte // the master's CA -- the node's certificate must chain to it
	Cert    []byte // the master's client certificate
	Key     []byte

	// Timeout bounds a single request. Provision is exempt: it can legitimately
	// run for minutes while a kernel installs, so it uses the caller's context
	// instead.
	Timeout time.Duration
}

const defaultClientTimeout = 30 * time.Second

func NewClient(cfg ClientConfig) (*Client, error) {
	tlsCfg, err := MasterTLSConfig(cfg.CACert, cfg.Cert, cfg.Key, cfg.Address)
	if err != nil {
		return nil, err
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = defaultClientTimeout
	}
	return &Client{
		base: "https://" + net.JoinHostPort(cfg.Address, strconv.Itoa(cfg.Port)),
		http: &http.Client{
			Timeout:   timeout,
			Transport: &http.Transport{TLSClientConfig: tlsCfg},
		},
	}, nil
}

// do performs one request and decodes either the result or the node's error.
//
// A non-2xx carrying an error body is turned back into a Go error with the
// node's own message. Reporting "500 from the node" instead would lose the one
// piece of information that makes a remote failure diagnosable at all.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e errorBody
		if json.NewDecoder(resp.Body).Decode(&e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("node: %s %s: %s", method, path, resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) Status(ctx context.Context) (NodeStatus, error) {
	var st NodeStatus
	err := c.do(ctx, http.MethodGet, "/status", nil, &st)
	return st, err
}

func (c *Client) Apply(ctx context.Context, state DesiredState) (ApplyResult, error) {
	var res ApplyResult
	err := c.do(ctx, http.MethodPost, "/apply", state, &res)
	return res, err
}

func (c *Client) Collect(ctx context.Context) (CollectResult, error) {
	var res CollectResult
	err := c.do(ctx, http.MethodPost, "/collect", nil, &res)
	return res, err
}

func (c *Client) Enforce(ctx context.Context, disabled DisabledSet) error {
	return c.do(ctx, http.MethodPost, "/enforce", disabled, nil)
}

func (c *Client) Logs(ctx context.Context, core string) (string, error) {
	var out struct {
		Logs string `json:"logs"`
	}
	err := c.do(ctx, http.MethodGet, "/logs?core="+core, nil, &out)
	return out.Logs, err
}

// Provision streams steps back as they happen.
//
// It deliberately does not use c.http, whose timeout would cut a legitimate
// kernel install off mid-way. The caller's context is the only deadline, which
// matches what the operator sees: provisioning ends when it ends, or when they
// navigate away.
func (c *Client) Provision(ctx context.Context, cores []string) (<-chan ProvisionStep, error) {
	body, err := json.Marshal(struct {
		Cores []string `json:"cores"`
	}{Cores: cores})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/provision", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	streaming := &http.Client{Transport: c.http.Transport} // no Timeout
	resp, err := streaming.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		var e errorBody
		if json.NewDecoder(resp.Body).Decode(&e) == nil && e.Error != "" {
			return nil, errors.New(e.Error)
		}
		return nil, fmt.Errorf("node: provision: %s", resp.Status)
	}

	ch := make(chan ProvisionStep, 32)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		// Provisioning captures raw package-manager output into Step.Log, which
		// runs well past bufio's default 64KB line cap; without this the stream
		// stops silently part-way through an install.
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			line := bytes.TrimSpace(sc.Bytes())
			if len(line) == 0 {
				continue
			}
			var step ProvisionStep
			if err := json.Unmarshal(line, &step); err != nil {
				continue
			}
			select {
			case ch <- step:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}
