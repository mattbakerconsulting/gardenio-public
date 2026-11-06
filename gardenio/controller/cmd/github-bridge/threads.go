package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// reviewThreadsQuery reads a PR's state and its review threads. isResolved is
// GitHub's own "Resolve conversation" bit, the same one branch protection
// counts. Only the first comment of each thread is read: it is the reviewer's
// ask; the replies are the conversation around it. authorAssociation is what
// Threads gates on: an outsider's thread must not respawn an agent with its
// text as the prompt (gd-b5drtT0ymT).
const reviewThreadsQuery = `query($owner:String!,$name:String!,$number:Int!,$after:String){
  repository(owner:$owner,name:$name){
    pullRequest(number:$number){
      state title headRefName
      reviewThreads(first:100,after:$after){
        pageInfo{hasNextPage endCursor}
        nodes{
          id isResolved isOutdated path line originalLine
          comments(first:1){nodes{body url authorAssociation author{login}}}
        }
      }
    }
  }
}`

// maxThreadPages bounds the scan at 1000 threads, far past any PR an agent
// works; beyond it the resolver acts on what it has seen.
const maxThreadPages = 10

// graphQLThreads implements threadSource over the GitHub GraphQL API with a
// per-repo installation token.
type graphQLThreads struct {
	*githubCommenter
}

// newGraphQLThreads builds the source on the same token source and HTTP plumbing
// the squawk commenter uses. GitHub Enterprise Server serves REST at /api/v3 and
// GraphQL at /api/graphql, so the REST base is mapped rather than appended to.
func newGraphQLThreads(tokens *tokenSource, apiURL string) *graphQLThreads {
	c := newGitHubCommenter(tokens, apiURL)
	if base, ok := strings.CutSuffix(c.baseURL, "/api/v3"); ok {
		c.baseURL = base + "/api"
	}
	return &graphQLThreads{c}
}

type threadsResponse struct {
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
	Data struct {
		Repository *struct {
			PullRequest *struct {
				State         string `json:"state"`
				Title         string `json:"title"`
				HeadRefName   string `json:"headRefName"`
				ReviewThreads struct {
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
					Nodes []struct {
						ID           string `json:"id"`
						IsResolved   bool   `json:"isResolved"`
						IsOutdated   bool   `json:"isOutdated"`
						Path         string `json:"path"`
						Line         int    `json:"line"`
						OriginalLine int    `json:"originalLine"`
						Comments     struct {
							Nodes []struct {
								Body              string `json:"body"`
								URL               string `json:"url"`
								AuthorAssociation string `json:"authorAssociation"`
								Author            *struct {
									Login string `json:"login"`
								} `json:"author"`
							} `json:"nodes"`
						} `json:"comments"`
					} `json:"nodes"`
				} `json:"reviewThreads"`
			} `json:"pullRequest"`
		} `json:"repository"`
	} `json:"data"`
}

// Threads returns the PR's state and its unresolved threads. A thread whose
// first comment is the bridge's own squawk marker is skipped: it is the agent
// talking, not a reviewer asking. So is a thread opened by anyone who is not a
// trusted author (trustedAssociation): it is not a reviewer either.
func (g *graphQLThreads) Threads(ctx context.Context, pr prRef) (*prThreads, error) {
	owner, name, _ := strings.Cut(pr.Repo, "/")
	out := &prThreads{}
	after := any(nil)
	for page := 0; page < maxThreadPages; page++ {
		var resp threadsResponse
		body := map[string]any{
			"query":     reviewThreadsQuery,
			"variables": map[string]any{"owner": owner, "name": name, "number": pr.Number, "after": after},
		}
		if err := g.do(ctx, pr.Repo, http.MethodPost, "/graphql", body, http.StatusOK, &resp); err != nil {
			return nil, err
		}
		// GraphQL reports failures with a 200 and an errors array.
		if len(resp.Errors) > 0 {
			return nil, fmt.Errorf("graphql: %s", resp.Errors[0].Message)
		}
		if resp.Data.Repository == nil || resp.Data.Repository.PullRequest == nil {
			return nil, errors.New("graphql: pull request not found")
		}
		p := resp.Data.Repository.PullRequest
		out.State, out.Title, out.Branch = p.State, p.Title, p.HeadRefName
		for _, n := range p.ReviewThreads.Nodes {
			if n.IsResolved || len(n.Comments.Nodes) == 0 {
				continue
			}
			c := n.Comments.Nodes[0]
			if strings.Contains(c.Body, squawkMarkerPrefix) || !trustedAssociation(c.AuthorAssociation) {
				continue
			}
			t := reviewThread{
				ID: n.ID, Path: n.Path, Line: n.Line, Outdated: n.IsOutdated,
				Body: c.Body, URL: c.URL,
			}
			if t.Line == 0 {
				t.Line = n.OriginalLine
			}
			if c.Author != nil {
				t.Author = c.Author.Login
			}
			out.Unresolved = append(out.Unresolved, t)
		}
		if !p.ReviewThreads.PageInfo.HasNextPage {
			break
		}
		after = p.ReviewThreads.PageInfo.EndCursor
	}
	return out, nil
}

var _ threadSource = (*graphQLThreads)(nil)
