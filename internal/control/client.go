package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/erinwnash/Shepherd/internal/db"
	"github.com/erinwnash/Shepherd/internal/eventlog"
)

// Client is a control socket client communicating over a local Unix domain socket.
type Client struct {
	httpClient *http.Server
	client     *http.Client
	token      string
}

// NewClient initializes a new Client targeting the specified Unix socket path.
func NewClient(socketPath string, token string) *Client {
	if socketPath == "" {
		socketPath = DefaultSocketPath()
	}

	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
	}

	return &Client{
		client: &http.Client{
			Transport: transport,
			Timeout:   30 * time.Second,
		},
		token: token,
	}
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, "http://unix"+path, bodyReader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var errPayload struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(respBytes, &errPayload) == nil && errPayload.Error != "" {
			return errors.New(errPayload.Error)
		}
		return fmt.Errorf("server returned HTTP %d: %s", resp.StatusCode, string(respBytes))
	}

	if out != nil {
		return json.Unmarshal(respBytes, out)
	}
	return nil
}

func (c *Client) Status(ctx context.Context) (DaemonStatus, error) {
	var st DaemonStatus
	err := c.do(ctx, http.MethodGet, "/api/v1/status", nil, &st)
	return st, err
}

func (c *Client) Start(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/api/v1/start", nil, nil)
}

func (c *Client) Pause(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/api/v1/pause", nil, nil)
}

func (c *Client) Resume(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/api/v1/resume", nil, nil)
}

func (c *Client) Stop(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/api/v1/stop", nil, nil)
}

func (c *Client) Halt(ctx context.Context, reason string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/halt", HaltRequest{Reason: reason}, nil)
}

func (c *Client) Drain(ctx context.Context, enabled bool, until *time.Time) error {
	return c.do(ctx, http.MethodPost, "/api/v1/drain", DrainRequest{Enabled: enabled, Until: until}, nil)
}

func (c *Client) Lockdown(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/api/v1/lockdown", nil, nil)
}

func (c *Client) Unlock(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/api/v1/unlock", nil, nil)
}

// RotateNtfyTopic switches notifications (and the ntfy command listener) to a fresh random topic.
func (c *Client) RotateNtfyTopic(ctx context.Context) (string, error) {
	var out struct {
		Topic string `json:"ntfy_topic"`
	}
	err := c.do(ctx, http.MethodPost, "/api/v1/notify/rotate-topic", nil, &out)
	return out.Topic, err
}

func (c *Client) ConfigGet(ctx context.Context) (map[string]string, error) {
	var out map[string]string
	err := c.do(ctx, http.MethodGet, "/api/v1/config", nil, &out)
	return out, err
}

func (c *Client) ConfigSet(ctx context.Context, key, val string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/config/set", ConfigSetRequest{Key: key, Value: val}, nil)
}

func (c *Client) ConfigReload(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/api/v1/config/reload", nil, nil)
}

func (c *Client) QueueList(ctx context.Context) ([]db.ChunkRecord, error) {
	var out []db.ChunkRecord
	err := c.do(ctx, http.MethodGet, "/api/v1/queue", nil, &out)
	return out, err
}

func (c *Client) QueueSkip(ctx context.Context, chunkID string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/queue/skip", QueueActionRequest{ChunkID: chunkID}, nil)
}

func (c *Client) QueueRetry(ctx context.Context, chunkID string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/queue/retry", QueueActionRequest{ChunkID: chunkID}, nil)
}

func (c *Client) QueueHold(ctx context.Context, chunkID string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/queue/hold", QueueActionRequest{ChunkID: chunkID}, nil)
}

func (c *Client) QueueRelease(ctx context.Context, chunkID string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/queue/release", QueueActionRequest{ChunkID: chunkID}, nil)
}

func (c *Client) QueuePin(ctx context.Context, chunkID, tier, pool string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/queue/pin", QueuePinRequest{ChunkID: chunkID, Tier: tier, Pool: pool}, nil)
}

// QueueSetActive checks (true) or unchecks (false) one queued chunk.
func (c *Client) QueueSetActive(ctx context.Context, chunkID string, active bool) error {
	return c.do(ctx, http.MethodPost, "/api/v1/queue/active", QueueActiveRequest{ChunkID: chunkID, Active: active}, nil)
}

// QueueSetAllActive checks or unchecks every queued chunk and returns how many changed.
func (c *Client) QueueSetAllActive(ctx context.Context, active bool) (int, error) {
	var out QueueActiveAllResult
	err := c.do(ctx, http.MethodPost, "/api/v1/queue/active-all", QueueActiveAllRequest{Active: active}, &out)
	return out.Changed, err
}

// QueueAssign routes a chunk to a tier/model regardless of complexity; an empty tier clears it.
func (c *Client) QueueAssign(ctx context.Context, chunkID, tier, model string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/queue/assign", QueueAssignRequest{ChunkID: chunkID, Tier: tier, Model: model}, nil)
}

// QueueRefresh re-reads the plan tree from disk.
func (c *Client) QueueRefresh(ctx context.Context) (QueueRefreshResult, error) {
	var out QueueRefreshResult
	err := c.do(ctx, http.MethodPost, "/api/v1/queue/refresh", nil, &out)
	return out, err
}

// FrontRunStart starts the front-run lane on a tier (which may be switched off) and optional model.
func (c *Client) FrontRunStart(ctx context.Context, tier, model string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/frontrun/start", FrontRunStartRequest{Tier: tier, Model: model}, nil)
}

// FrontRunStop stops the front-run lane; its current chunk goes back to the main queue.
func (c *Client) FrontRunStop(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/api/v1/frontrun/stop", nil, nil)
}

// QueueCheck spot-tests one chunk against the disk and repairs its row if its work order is done.
func (c *Client) QueueCheck(ctx context.Context, chunkID string) (QueueCheckResult, error) {
	var out QueueCheckResult
	err := c.do(ctx, http.MethodPost, "/api/v1/queue/check", QueueActionRequest{ChunkID: chunkID}, &out)
	return out, err
}

func (c *Client) QueueDiff(ctx context.Context, chunkID string) (string, error) {
	v := url.Values{}
	v.Set("chunk_id", chunkID)
	var out struct {
		Diff string `json:"diff"`
	}
	err := c.do(ctx, http.MethodGet, "/api/v1/queue/diff?"+v.Encode(), nil, &out)
	return out.Diff, err
}

func (c *Client) QueueApprove(ctx context.Context, chunkID string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/queue/approve", QueueActionRequest{ChunkID: chunkID}, nil)
}

func (c *Client) QueueReject(ctx context.Context, chunkID, reason string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/queue/reject", QueueRejectRequest{ChunkID: chunkID, Reason: reason}, nil)
}

func (c *Client) QuotaGet(ctx context.Context) (map[string]any, error) {
	var out map[string]any
	err := c.do(ctx, http.MethodGet, "/api/v1/quota", nil, &out)
	return out, err
}

func (c *Client) QuotaSetFloor(ctx context.Context, pool string, pct int) error {
	return c.do(ctx, http.MethodPost, "/api/v1/quota/set-floor", QuotaSetPctRequest{Pool: pool, Pct: pct}, nil)
}

func (c *Client) QuotaSetReserve(ctx context.Context, pool string, pct int) error {
	return c.do(ctx, http.MethodPost, "/api/v1/quota/set-reserve", QuotaSetPctRequest{Pool: pool, Pct: pct}, nil)
}

func (c *Client) QuotaTierToggle(ctx context.Context, tier string, enabled bool) error {
	return c.do(ctx, http.MethodPost, "/api/v1/quota/tier", QuotaTierRequest{Tier: tier, Enabled: enabled}, nil)
}

func (c *Client) QuotaResetBucket(ctx context.Context, pool string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/quota/reset-bucket", QuotaResetBucketRequest{Pool: pool}, nil)
}

func (c *Client) Audit(ctx context.Context, limit int) ([]eventlog.Event, error) {
	var out []eventlog.Event
	v := url.Values{}
	if limit > 0 {
		v.Set("limit", strconv.Itoa(limit))
	}
	path := "/api/v1/audit"
	if len(v) > 0 {
		path += "?" + v.Encode()
	}
	err := c.do(ctx, http.MethodGet, path, nil, &out)
	return out, err
}
