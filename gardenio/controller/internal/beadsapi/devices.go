package beadsapi

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// Device mirrors kbeads' push device registry row (migration 053, Lands 4.7b
// gd-a7U4AgwgCA). UserEmail is the human who registered the install; the push
// bridge alerts a device only for alerts addressed to that human.
type Device struct {
	Token      string    `json:"token"`
	Platform   string    `json:"platform"`
	BundleID   string    `json:"bundle_id,omitempty"`
	UserEmail  string    `json:"user_email"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
}

type listDevicesResponse struct {
	Devices []*Device `json:"devices"`
}

// ListDevices returns registered push devices owned by any of userEmails,
// optionally narrowed to one platform. An OIDC caller without admin sees only
// their own devices; an OIDC admin and the push bridge's machine principal
// (machine:gardenio-push-bridge) see any; every other caller gets 403.
//
// An empty userEmails means EVERY owner, so a caller resolving an alert's
// recipients must not call this with an empty list.
func (c *Client) ListDevices(ctx context.Context, userEmails []string, platform string) ([]*Device, error) {
	q := url.Values{}
	for _, e := range userEmails {
		if e != "" {
			q.Add("user_email", e)
		}
	}
	if platform != "" {
		q.Set("platform", platform)
	}
	path := "/v1/devices"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var resp listDevicesResponse
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Devices, nil
}

// DeleteDevice removes a device from the registry. Used by the push
// bridge after APNs reports the token is invalid (HTTP 410 Gone) so we
// don't keep retrying dead tokens.
func (c *Client) DeleteDevice(ctx context.Context, token string) error {
	return c.doJSON(ctx, http.MethodDelete, "/v1/devices/"+url.PathEscape(token), nil, nil)
}
