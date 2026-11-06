package beadsapi

import (
	"context"
	"net/http"
	"net/url"
)

// Land members (Lands Commons III step 6): the people of one land, for the
// Commons' @ list. kbeads answers GET /v1/lands/{land}/members only to a
// caller whose scope admits the land, and 404 for any other land, so with
// the viewer's bearer on ctx the list can only be of the viewer's own land.

// LandMember is one person of a land.
type LandMember struct {
	// Principal is oidc:<email>, as conversation beads name a person.
	Principal string `json:"principal"`
	// Name is the person's user bead title.
	Name string `json:"name"`
	// Role is the person's tier in the land (owner, member or guest) as of
	// their last sign-in, or "" when their user bead has not recorded one.
	// It is for showing and addressing people; no access decision reads it.
	Role string `json:"role,omitempty"`
}

type landMembersBody struct {
	Land    string       `json:"land"`
	Members []LandMember `json:"members"`
}

// LandMembers returns the people of land.
func (c *Client) LandMembers(ctx context.Context, land string) ([]LandMember, error) {
	var resp landMembersBody
	if err := c.doJSON(ctx, http.MethodGet, "/v1/lands/"+url.PathEscape(land)+"/members", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Members, nil
}
