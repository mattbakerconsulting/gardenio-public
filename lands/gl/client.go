package main

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"gardenio/lands/contract/landsv1"

	"github.com/spf13/cobra"
)

// landConn is a connection to one land's door.
type landConn struct {
	a    *app
	land string
	edge string
	base string
	c    *landsv1.ClientWithResponses
}

// connect resolves the land and the edge and builds the contract client.
func (a *app) connect() (*landConn, error) {
	land, err := a.land()
	if err != nil {
		return nil, err
	}
	edge, err := a.edge()
	if err != nil {
		return nil, err
	}
	a.banner(land)
	l := &landConn{a: a, land: land, edge: edge, base: baseURL(edge, land)}
	c, err := landsv1.NewClientWithResponses(l.base, landsv1.WithHTTPClient(&doer{l: l, hc: a.hc}))
	if err != nil {
		return nil, err
	}
	l.c = c
	return l, nil
}

// doer sends a contract request with the land's bearer. When the land
// answers 401 it renews the sign-in once and resends: the same request, so a
// POST keeps its Idempotency-Key and cannot act twice.
type doer struct {
	l  *landConn
	hc *http.Client
}

func (d *doer) Do(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	tok, err := d.l.a.bearer(ctx, d.l.edge, d.l.land, false)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := d.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized && (req.Body == nil || req.GetBody != nil) {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
		tok, err := d.l.a.bearer(ctx, d.l.edge, d.l.land, true)
		if err != nil {
			return nil, err
		}
		retry := req.Clone(ctx)
		if req.GetBody != nil {
			if retry.Body, err = req.GetBody(); err != nil {
				return nil, err
			}
		}
		retry.Header.Set("Authorization", "Bearer "+tok)
		if resp, err = d.hc.Do(retry); err != nil {
			return nil, err
		}
	}
	d.l.a.checkContract(resp)
	return resp, nil
}

// checkContract warns once when the land speaks another contract version.
// gl carries on: the contract is additive, and unknown fields and enum
// values are read as absent and "other".
func (a *app) checkContract(resp *http.Response) {
	v := resp.Header.Get(landsv1.HeaderContract)
	if v == "" || v == landsv1.ContractVersion || a.skewWarned {
		return
	}
	a.skewWarned = true
	fmt.Fprintf(a.errOut, "gl: the land speaks Lands contract %s and this gl was built for %s; carrying on (update gl if something looks wrong)\n", v, landsv1.ContractVersion)
}

// problemError is the land's answer to a request it would not carry out.
type problemError struct {
	p    landsv1.Problem
	hint string
}

func (e *problemError) Error() string {
	var b strings.Builder
	b.WriteString(e.p.Title)
	if e.p.Detail != nil && *e.p.Detail != "" {
		b.WriteString(": " + *e.p.Detail)
	}
	if e.hint != "" {
		b.WriteString("; " + e.hint)
	}
	fmt.Fprintf(&b, " (%s", e.p.Code)
	if e.p.CorrelationId != "" {
		fmt.Fprintf(&b, "; quote %s to support", e.p.CorrelationId)
	}
	b.WriteString(")")
	return b.String()
}

// fail turns a response that was not the expected success into an error.
func (l *landConn) fail(status int, p *landsv1.Problem) error {
	if p != nil && p.Code != "" {
		pe := &problemError{p: *p}
		switch p.Code {
		case landsv1.Unauthenticated:
			pe.hint = "run '" + l.a.loginHint(l.land) + "'"
		case landsv1.RateLimited:
			pe.hint = "try again in a moment"
		}
		return pe
	}
	switch status {
	case http.StatusUnauthorized:
		return &signInError{fmt.Sprintf("land %s did not accept your sign-in: run '%s'", l.land, l.a.loginHint(l.land))}
	case http.StatusForbidden:
		return fmt.Errorf("land %s turned the request away (403); are you a member of it?", l.land)
	case http.StatusNotFound:
		return fmt.Errorf("no land %s at %s (404)", l.land, l.edge)
	}
	return fmt.Errorf("land %s answered %d %s", l.land, status, http.StatusText(status))
}

// emit prints v as JSON with --json, otherwise through human.
func (a *app) emit(v any, human func(io.Writer)) error {
	if a.jsonOut {
		enc := json.NewEncoder(a.out)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	human(a.out)
	return nil
}

// newKey is a fresh Idempotency-Key: every POST gl sends carries one, so a
// resend (after a 401, or by the transport) never acts twice.
func newKey() *landsv1.IdempotencyKeyHeader {
	k := rand.Text()
	return &k
}

// paging is the --limit and --cursor every list takes.
type paging struct {
	limit  int
	cursor string
}

func (p *paging) flags(cmd *cobra.Command) {
	cmd.Flags().IntVar(&p.limit, "limit", 0, "at most this many (1-200; the land's default is 50)")
	cmd.Flags().StringVar(&p.cursor, "cursor", "", "continue from where the previous page stopped")
}

func (p *paging) limitPtr() *int {
	if p.limit == 0 {
		return nil
	}
	return &p.limit
}

func (p *paging) cursorPtr() *string {
	if p.cursor == "" {
		return nil
	}
	return &p.cursor
}

// moreHint tells the person how to read the next page.
func (a *app) moreHint(next *string) {
	if next != nil && *next != "" && !a.jsonOut {
		fmt.Fprintf(a.errOut, "(more: --cursor %s)\n", *next)
	}
}
