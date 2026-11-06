package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"

	"github.com/spf13/cobra"
)

// commonsCmd reads the Commons from inside an agent pod (Lands spec
// gardenio/docs/superpowers/specs/2026-10-19-lands-commons-design.md).
var commonsCmd = &cobra.Command{
	Use:     "commons",
	Short:   "Read the Commons: fetch a message's attachments",
	GroupID: "orchestration",
}

// commons-file flags.
var (
	commonsFileOut       string
	commonsFileViewerURL string
)

var commonsFileCmd = &cobra.Command{
	Use:   "file <message-id|file-url>",
	Short: "Fetch a Commons message's attachments to local files",
	Long: `Fetch the files attached to a Commons message, so you can Read them.

A Commons attachment's link (…/garden/c/f/…) opens only in a signed-in browser.
This fetches the bytes from beads-viewer's machine file route on your own
token, which reads the channels of your own land only.

<message-id> is the message bead a nudge names ("fetch with: gb commons file
<id>"): every file in its files field is fetched. A file URL fetches that one
file.

Files are written under --out: a directory, or for one file a file path. The
default is a directory per message in your scratch directory
($CLAUDE_CODE_TMPDIR, else the system temp dir). Each path is printed, one per
line. --json prints the message id, each file's name, mimetype, size, url and
path, and the message's snap (a farm snapshot's context) when it has one.

The viewer is BEADS_VIEWER_INTERNAL_URL, its in-cluster Service, which the
controller sets in every agent pod; --viewer-url overrides it.

Examples:
  gb commons file gd-abc123
  gb commons file gd-abc123 --json
  gb commons file https://gardenio.example/beads/garden/c/f/gardenio/gd-ch1/<id>/farm.png --out farm.png`,
	Args: cobra.ExactArgs(1),
	RunE: runCommonsFile,
}

func init() {
	commonsFileCmd.Flags().StringVarP(&commonsFileOut, "out", "o", "", "directory to write into, or a file path for one file (default: a directory in the scratch dir)")
	commonsFileCmd.Flags().StringVar(&commonsFileViewerURL, "viewer-url", "", "beads-viewer in-cluster base URL (default $BEADS_VIEWER_INTERNAL_URL)")
	commonsCmd.AddCommand(commonsFileCmd)
	rootCmd.AddCommand(commonsCmd)
}

// commonsFileMax caps one fetched file: the Commons' own cap on an attachment.
const commonsFileMax = 10 << 20

// commonsFetchTimeout bounds the whole fetch.
const commonsFetchTimeout = 2 * time.Minute

// commonsFileRef is one entry of a message bead's files field.
type commonsFileRef struct {
	Name     string `json:"name"`
	MimeType string `json:"mimetype,omitempty"`
	Size     int64  `json:"size,omitempty"`
	URL      string `json:"url"`
}

// commonsFetched is one file written to disk.
type commonsFetched struct {
	Name     string `json:"name"`
	MimeType string `json:"mimetype,omitempty"`
	Size     int64  `json:"size"`
	URL      string `json:"url"`
	Path     string `json:"path"`
}

// commonsFileResult is what --json prints.
type commonsFileResult struct {
	Message string           `json:"message,omitempty"`
	Files   []commonsFetched `json:"files"`
	Snap    json.RawMessage  `json:"snap,omitempty"`
}

// commonsFileFetch is one run of gb commons file, with its dependencies.
type commonsFileFetch struct {
	viewer  string // in-cluster base URL, no trailing slash
	token   string
	out     string
	tmp     string // the default scratch dir
	getBead func(ctx context.Context, id string) (*beadsapi.BeadDetail, error)
	client  *http.Client
}

func runCommonsFile(cmd *cobra.Command, args []string) error {
	viewer := strings.TrimRight(strings.TrimSpace(firstNonEmpty(commonsFileViewerURL, os.Getenv("BEADS_VIEWER_INTERNAL_URL"))), "/")
	if viewer == "" {
		return errors.New("no viewer: set BEADS_VIEWER_INTERNAL_URL (the controller sets it in agent pods) or pass --viewer-url")
	}
	token := tokenFlag
	if token == "" {
		token = defaultToken()
	}
	if token == "" {
		return errors.New("no token: gb commons file reads on your own agent token (BEADS_DAEMON_TOKEN), and none is set")
	}
	tmp := os.Getenv(envClaudeCodeTmpDir)
	if tmp == "" {
		tmp = os.TempDir()
	}
	f := commonsFileFetch{
		viewer: viewer, token: token, out: commonsFileOut, tmp: tmp,
		getBead: daemon.GetBead, client: &http.Client{Timeout: commonsFetchTimeout},
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), commonsFetchTimeout)
	defer cancel()
	res, err := f.run(ctx, args[0])
	if err != nil {
		return err
	}
	if jsonOutput {
		printJSON(res)
		return nil
	}
	for _, file := range res.Files {
		fmt.Fprintln(cmd.OutOrStdout(), file.Path)
	}
	return nil
}

// run fetches the files arg names: a message bead's files field, or one file
// URL.
func (f commonsFileFetch) run(ctx context.Context, arg string) (commonsFileResult, error) {
	var res commonsFileResult
	var refs []commonsFileRef
	if strings.HasPrefix(arg, "http://") || strings.HasPrefix(arg, "https://") {
		refs = []commonsFileRef{{URL: arg}}
	} else {
		bead, err := f.getBead(ctx, arg)
		if err != nil {
			return res, fmt.Errorf("reading message %s: %w", arg, err)
		}
		if bead.Type != "message" {
			return res, fmt.Errorf("%s is a %s bead, not a Commons message", arg, bead.Type)
		}
		raw := strings.TrimSpace(bead.Fields["files"])
		if raw == "" {
			return res, fmt.Errorf("message %s has no files", arg)
		}
		if err := json.Unmarshal([]byte(raw), &refs); err != nil {
			return res, fmt.Errorf("message %s: unreadable files field: %w", arg, err)
		}
		res.Message = bead.ID
		if snap := strings.TrimSpace(bead.Fields["snap"]); snap != "" && json.Valid([]byte(snap)) {
			res.Snap = json.RawMessage(snap)
		}
	}

	type planned struct {
		ref     commonsFileRef
		machine string // the machine route URL
		name    string // the stored name, from the URL: safe as a file name
	}
	var plan []planned
	for _, ref := range refs {
		_, _, machinePath, ok := bridge.CommonsFileRoute(ref.URL)
		if !ok {
			if res.Message == "" {
				return res, fmt.Errorf("%s is not a Commons file link (…/garden/c/f/<land>/<channel>/<file>/<name>)", ref.URL)
			}
			continue // a Slack permalink or another host's link: not ours to fetch
		}
		name, err := url.PathUnescape(path.Base(machinePath))
		if err != nil || !safeFileName(name) {
			return res, fmt.Errorf("%s: the file name is not safe to write", ref.URL)
		}
		plan = append(plan, planned{ref: ref, machine: f.viewer + machinePath, name: name})
	}
	if len(plan) == 0 {
		return res, fmt.Errorf("message %s has no Commons attachments (its files are links elsewhere)", arg)
	}

	dest := func(name string) string { return filepath.Join(f.out, name) }
	switch {
	case f.out == "":
		dir := filepath.Join(f.tmp, "commons-files", firstNonEmpty(res.Message, "file"))
		dest = func(name string) string { return filepath.Join(dir, name) }
	case len(plan) == 1 && !isDir(f.out) && !strings.HasSuffix(f.out, string(os.PathSeparator)):
		dest = func(string) string { return f.out }
	}

	for _, p := range plan {
		target := dest(p.name)
		got, err := f.fetch(ctx, p.machine, target)
		if err != nil {
			return res, fmt.Errorf("%s: %w", p.name, err)
		}
		res.Files = append(res.Files, commonsFetched{
			Name: firstNonEmpty(p.ref.Name, p.name), MimeType: firstNonEmpty(p.ref.MimeType, got.mimeType),
			Size: got.size, URL: p.ref.URL, Path: target,
		})
	}
	return res, nil
}

type fetchedFile struct {
	mimeType string
	size     int64
}

// fetch GETs one file from the machine route on the agent's token and writes
// it to target.
func (f commonsFileFetch) fetch(ctx context.Context, machineURL, target string) (fetchedFile, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, machineURL, nil)
	if err != nil {
		return fetchedFile{}, err
	}
	req.Header.Set("Authorization", "Bearer "+f.token)
	resp, err := f.client.Do(req)
	if err != nil {
		return fetchedFile{}, fmt.Errorf("fetching from the viewer: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		msg := strings.TrimSpace(string(body))
		if resp.StatusCode == http.StatusForbidden {
			msg += " (an agent reads only its own land's files, on its own per-agent token)"
		}
		return fetchedFile{}, fmt.Errorf("the viewer answered %d: %s", resp.StatusCode, msg)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return fetchedFile{}, err
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fetchedFile{}, err
	}
	n, err := io.Copy(out, io.LimitReader(resp.Body, commonsFileMax+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > commonsFileMax {
		err = errors.New("larger than 10 MB")
	}
	if err != nil {
		_ = os.Remove(target)
		return fetchedFile{}, err
	}
	return fetchedFile{mimeType: resp.Header.Get("Content-Type"), size: n}, nil
}

// safeFileName reports whether name is one plain path element.
func safeFileName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, `/\`) && !strings.ContainsRune(name, 0)
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}
