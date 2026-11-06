package model

import "time"

// PushDevicePlatformIOS is the only platform the push bridge delivers to
// today (APNs).
const PushDevicePlatformIOS = "ios"

// PushDevice is one row of push_devices (migration 053, Lands 4.7b
// gd-a7U4AgwgCA): an app install that receives native push alerts for the
// human who registered it. UserEmail is the lowercase OIDC subject of the
// registering token; the daemon never takes it from the request body.
type PushDevice struct {
	Token      string    `json:"token"`
	Platform   string    `json:"platform"`
	BundleID   string    `json:"bundle_id,omitempty"`
	UserEmail  string    `json:"user_email"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
}

// PushDeviceFilter narrows ListPushDevices. UserEmails matches any of the
// given owners (empty = every owner); Platform matches exactly when set.
type PushDeviceFilter struct {
	UserEmails []string
	Platform   string
}
