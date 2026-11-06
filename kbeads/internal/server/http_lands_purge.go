package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
)

// POST /v1/lands/purge (Lands Track B spec §3.3, step 8.11) hard-deletes what
// the privacy policy (gardenio/docs/lands/legal/privacy.md §5) says goes after
// a land closes or an application is turned down. The controller's purge
// sweep (internal/lands/purge) decides when to call; the daemon refuses
// anything that may not go yet, so a misconfigured or compromised caller
// cannot delete early:
//
//   - {"land": "<land>"}: every bead labelled project:<land>, soft-deleted ones
//     too, except the project bead itself (the tombstone that keeps the name
//     and prefix taken and records land_purged_at), the credit ledger
//     (credit_grant), which the policy keeps for the records period, and any
//     bead that also carries another project's label (it is not the land's
//     alone). The land's project bead must be a closed tenant land whose
//     land_closed_at is at least BEADS_LANDS_RETENTION_DAYS old.
//   - {"land": "<land>", "include_records": true}: the same plus the ledger,
//     once land_closed_at is BEADS_LANDS_RECORDS_RETENTION_DAYS old.
//   - {"application_id": "<id>"}: one plot_application in lands-intake whose
//     state is denied or withdrawn, decided (or closed)
//     BEADS_LANDS_APPLICATION_RETENTION_DAYS ago.
//
// A land purge is refused too when the label reaches beads that are not the
// land's (notTheLandsAlone, a second project bead with the name): the purge
// deletes by label, and a land may share its name with an older project.
//
// "dry_run": true lists what would go and deletes nothing; it is refused the
// same way, so the caller learns before it deletes anything of its own (the
// controller deletes session objects between the two calls). The reply is
// {"purged": [ids], "types": {id: type}}.
//
// Only an OIDC admin or a machine principal holding gardenio-machine-lands-
// provisioner (capLandPurge) may call it. The primary static token, which
// every agent pod holds, may not.
type landPurgeRequest struct {
	Land           string `json:"land"`
	ApplicationID  string `json:"application_id"`
	IncludeRecords bool   `json:"include_records"`
	DryRun         bool   `json:"dry_run"`
}

type landPurgeResponse struct {
	Purged []string          `json:"purged"`
	Types  map[string]string `json:"types"`
}

// landPurgeRecordTypes are kept until the records period is over.
var landPurgeRecordTypes = []string{string(typeCreditGrant)}

// plotPurgeableStates are the application states the purge may delete.
var plotPurgeableStates = []string{plotStateDenied, plotStateWithdrawn}

// The retention floors, in days, the daemon holds every purge to. The
// defaults are the privacy policy's recommended values
// (gardenio/docs/lands/legal/README.md: RETENTION_DAYS, RECORDS_RETENTION,
// APPLICATION_RETENTION); the controller's LANDS_*_RETENTION_DAYS may be
// longer, never shorter.
const (
	landsRetentionDaysEnv            = "BEADS_LANDS_RETENTION_DAYS"
	landsRecordsRetentionDaysEnv     = "BEADS_LANDS_RECORDS_RETENTION_DAYS"
	landsApplicationRetentionDaysEnv = "BEADS_LANDS_APPLICATION_RETENTION_DAYS"

	landsRetentionDefaultDays            = 30
	landsRecordsRetentionDefaultDays     = 730
	landsApplicationRetentionDefaultDays = 90
)

// landPurgeNow is the clock the retention floors are read against; tests
// replace it.
var landPurgeNow = time.Now

// landsRetentionDays reads a retention floor, def when unset. A value that
// is not a whole number of days, at least 1, falls back to def: a typo must
// never shorten what the policy promises.
func landsRetentionDays(key string, def int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		slog.Warn("lands purge: invalid retention, using default", "env", key, "value", raw, "default", def)
		return def
	}
	return n
}

// notYetPurgeable is the refusal for a target whose retention period has not
// passed (or that has no start to count it from), or "" when it has.
func notYetPurgeable(what string, since time.Time, key string, def int) string {
	if since.IsZero() {
		return what + " has no time its retention period starts from"
	}
	days := landsRetentionDays(key, def)
	due := since.AddDate(0, 0, days)
	if landPurgeNow().Before(due) {
		return fmt.Sprintf("%s may not be purged before %s (%s=%d)", what, due.UTC().Format(time.RFC3339), key, days)
	}
	return ""
}

func (s *BeadsServer) handleLandPurge(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !isOIDCAdminSession(ctx) && !machineHasCapability(ctx, capLandPurge) {
		writeError(w, http.StatusForbidden, "purging a land needs an OIDC gardenio-admin or the lands provisioner's machine principal")
		return
	}
	var req landPurgeRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	req.Land, req.ApplicationID = strings.TrimSpace(req.Land), strings.TrimSpace(req.ApplicationID)
	if (req.Land == "") == (req.ApplicationID == "") {
		writeError(w, http.StatusBadRequest, "exactly one of land and application_id is required")
		return
	}
	var (
		preq   store.PurgeRequest
		target string
		proj   *model.Bead
	)
	if req.Land != "" {
		var (
			status int
			msg    string
		)
		proj, status, msg = s.purgeableLand(r, req.Land, req.IncludeRecords)
		if msg != "" {
			writeError(w, status, msg)
			return
		}
		preq = store.PurgeRequest{Label: "project:" + req.Land, KeepIDs: []string{proj.ID}, KeepShared: true}
		if !req.IncludeRecords {
			preq.KeepTypes = landPurgeRecordTypes
		}
		target = "land " + req.Land
	} else {
		if status, msg := s.purgeableApplication(r, req.ApplicationID); msg != "" {
			writeError(w, status, msg)
			return
		}
		preq = store.PurgeRequest{IDs: []string{req.ApplicationID}}
		target = "application " + req.ApplicationID
	}

	// The selection is read first, so the reply carries each bead's type
	// (the controller deletes agents' session objects) and the delete takes
	// exactly what was listed.
	preq.DryRun = true
	beads, err := s.store.PurgeBeads(ctx, preq)
	if err != nil {
		writeServerError(ctx, w, "handleLandPurge", err, "failed to list the beads to purge")
		return
	}
	if proj != nil {
		if msg := notTheLandsAlone(req.Land, proj, beads); msg != "" {
			writeError(w, http.StatusConflict, msg)
			return
		}
	}
	if !req.DryRun && len(beads) > 0 {
		ids := make([]string, len(beads))
		for i, b := range beads {
			ids[i] = b.ID
		}
		preq.DryRun, preq.IDs, preq.Label, preq.KeepShared = false, ids, "", false
		if beads, err = s.store.PurgeBeads(ctx, preq); err != nil {
			writeServerError(ctx, w, "handleLandPurge", err, "failed to purge")
			return
		}
		actor, fleet := actorFromContext(ctx), FleetFromContext(ctx)
		for _, b := range beads {
			s.recordAndPublish(ctx, events.TopicBeadDeleted, b.ID, actor, events.BeadDeleted{BeadID: b.ID, Fleet: fleet})
		}
		slog.Info("lands purge: purged", "target", target, "beads", len(beads), "actor", actor,
			"include_records", req.IncludeRecords)
	}
	resp := landPurgeResponse{Purged: make([]string, 0, len(beads)), Types: make(map[string]string, len(beads))}
	for _, b := range beads {
		resp.Purged = append(resp.Purged, b.ID)
		resp.Types[b.ID] = b.Type
	}
	writeJSON(w, http.StatusOK, resp)
}

// purgeableLand returns the land's project bead when the land may be purged:
// a tenant land that is closed and stamped land_closed_at, longer ago than
// the retention floor (the records floor with includeRecords). Otherwise it
// returns the status and message to refuse with.
func (s *BeadsServer) purgeableLand(r *http.Request, land string, includeRecords bool) (*model.Bead, int, string) {
	if land == landsIntakeProject || strings.ContainsAny(land, " \t\n,:") {
		return nil, http.StatusBadRequest, "not a land name: " + land
	}
	proj, err := s.store.FindProjectByName(r.Context(), land)
	switch {
	case errors.Is(err, sql.ErrNoRows), err == nil && proj == nil:
		return nil, http.StatusNotFound, "no project named " + land
	case err != nil:
		slog.Error("lands purge: project lookup failed", "land", land, "error", err)
		return nil, http.StatusInternalServerError, "failed to read the land's project"
	}
	// A second project bead with the land's name (FindProjectByName ranks the
	// land first) means another project shares project:<land>.
	dups, _, err := s.store.ListBeads(r.Context(), model.BeadFilter{
		Type: []model.BeadType{"project"}, Titles: []string{land, "Project: " + land}, Limit: 10,
	})
	if err != nil {
		slog.Error("lands purge: duplicate project lookup failed", "land", land, "error", err)
		return nil, http.StatusInternalServerError, "failed to check the land's name is its own"
	}
	for _, d := range dups {
		if d.ID != proj.ID {
			return nil, http.StatusConflict, "project bead " + d.ID + " is also named " + land + "; the label project:" + land + " is not the land's alone, so an admin must sort the beads out by hand"
		}
	}
	var f struct {
		Kind     string `json:"land_kind"`
		ClosedAt string `json:"land_closed_at"`
	}
	if len(proj.Fields) > 0 {
		_ = json.Unmarshal(proj.Fields, &f)
	}
	if f.Kind != "tenant" {
		return nil, http.StatusConflict, "project " + land + " is not a tenant land; only a closed tenant land is purged"
	}
	if proj.Status != model.StatusClosed {
		return nil, http.StatusConflict, "land " + land + " is not closed"
	}
	closedAt, err := time.Parse(time.RFC3339, strings.TrimSpace(f.ClosedAt))
	if err != nil {
		return nil, http.StatusConflict, "land " + land + " has no land_closed_at; it was not closed by gb lands close"
	}
	msg := notYetPurgeable("land "+land, closedAt, landsRetentionDaysEnv, landsRetentionDefaultDays)
	if includeRecords {
		msg = notYetPurgeable("the records of land "+land, closedAt, landsRecordsRetentionDaysEnv, landsRecordsRetentionDefaultDays)
	}
	if msg != "" {
		return nil, http.StatusConflict, msg
	}
	return proj, 0, ""
}

// notTheLandsAlone refuses a land purge whose label selection reaches beads
// that are not the land's: another project bead, or a bead older than the
// land's own project bead, which the provisioner creates before any other
// bead of the land. Either means the name project:<land> was in use before
// or beside the tenant land (a fleet project, or one known only by its
// label), and deleting by the label would take that project's beads too
// (gd-i74esUHCTg). "" when every selected bead is the land's.
func notTheLandsAlone(land string, proj *model.Bead, beads []store.PurgedBead) string {
	for _, b := range beads {
		switch {
		case b.Type == "project":
			return fmt.Sprintf("project bead %s also carries project:%s; the label is not land %s's alone, so an admin must sort the beads out by hand", b.ID, land, land)
		case b.CreatedAt.Before(proj.CreatedAt):
			return fmt.Sprintf("bead %s carries project:%s but predates land %s's project bead %s; the label was in use before the land, so an admin must sort the beads out by hand", b.ID, land, land, proj.ID)
		}
	}
	return ""
}

// purgeableApplication checks that id is a denied or withdrawn plot
// application in lands-intake, decided longer ago than the application
// retention floor.
func (s *BeadsServer) purgeableApplication(r *http.Request, id string) (int, string) {
	b, err := s.store.GetBeadIncludingDeleted(r.Context(), id)
	switch {
	case errors.Is(err, sql.ErrNoRows), err == nil && b == nil:
		return http.StatusNotFound, "no bead " + id
	case err != nil:
		slog.Error("lands purge: application lookup failed", "application", id, "error", err)
		return http.StatusInternalServerError, "failed to read the application"
	}
	if b.Type != typePlotApplication || plotApplicationLabelError(b.Labels) != nil {
		return http.StatusConflict, id + " is not a plot application in " + landsIntakeProject
	}
	f, _ := decodePlotApplicationFields(b.Fields)
	if !slices.Contains(plotPurgeableStates, f.State) {
		return http.StatusConflict, "application " + id + " is " + f.State + "; only a denied or withdrawn application is purged"
	}
	if msg := notYetPurgeable("application "+id, applicationDecidedAt(b), landsApplicationRetentionDaysEnv, landsApplicationRetentionDefaultDays); msg != "" {
		return http.StatusConflict, msg
	}
	return 0, ""
}

// applicationDecidedAt is when an application was turned down or withdrawn:
// decided_at, else when it was closed, else its last update. Zero when it
// has none of them, which notYetPurgeable refuses.
func applicationDecidedAt(b *model.Bead) time.Time {
	var f struct {
		DecidedAt string `json:"decided_at"`
	}
	if len(b.Fields) > 0 {
		_ = json.Unmarshal(b.Fields, &f)
	}
	if t, err := time.Parse(time.RFC3339, strings.TrimSpace(f.DecidedAt)); err == nil {
		return t
	}
	if b.ClosedAt != nil {
		return *b.ClosedAt
	}
	return b.UpdatedAt
}
