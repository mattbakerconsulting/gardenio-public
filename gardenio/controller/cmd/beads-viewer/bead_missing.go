package main

import (
	"errors"
	"net/http"

	"gardenio/controller/internal/beadsapi"
)

// beadMissing reports whether a daemon read of a named bead failed because the
// bead is not there for this caller: a 404, or the 403 read-authz gives a
// scoped caller naming another land's bead. A loader answers both with its
// not-found response, so a page never tells a caller that another land's bead
// exists (spec §5.6; TestViewerCrossLandMatrix, gd-ZJOUPZWuvs).
func beadMissing(err error) bool {
	var apiErr *beadsapi.APIError
	return errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusNotFound || apiErr.StatusCode == http.StatusForbidden)
}
