package main

import (
	"strings"

	"gardenio/clicreds/credstore"
)

// operatedByFromCredstore returns "oidc:<email>" from the current credstore
// context, or "" if no email is captured. Mirrors gb's helper of the same
// name so kd stamps the same operator id gb would. Errors loading the
// credstore are swallowed — kd must still run when the credstore file is
// missing or unreadable (in-cluster, CI).
//
// kd reads only the identity from the credstore, never a credential: the
// cookie an older gb login left there is retired (gd-5FThTGbOOP) and kd
// authenticates with `kd login`, an env token or the active `kd remote`.
func operatedByFromCredstore() string {
	store, err := credstore.Load("")
	if err != nil {
		return ""
	}
	cur := store.Current()
	if cur == nil || cur.User == nil {
		return ""
	}
	email := strings.TrimSpace(cur.User.Email)
	if email == "" {
		return ""
	}
	return "oidc:" + email
}
