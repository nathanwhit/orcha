package forge

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
)

// This file implements the GitForge issue operations the issue-trigger monitor
// needs. They run on whatever executor the forge is bound to — for the trigger
// monitor that is the orchestrator host, where `gh` must be authenticated as the
// orcha bot account so it can read issues and post acknowledgements.

// ListRecentIssueComments returns the repo's most recent issue/PR conversation
// comments via the REST issues/comments endpoint (one call, newest first), so a
// single poll per repo can scan for @-mentions. The endpoint covers comments on
// both issues and PRs; PR comments are flagged via IsPR (their html_url points
// at /pull/ rather than /issues/) so the caller can skip them.
func (g *GitForge) ListRecentIssueComments(ctx context.Context, repo string) ([]IssueComment, error) {
	out, err := g.gh(ctx, "api",
		"repos/"+repo+"/issues/comments?sort=updated&direction=desc&per_page=100",
		"-H", "Accept: application/vnd.github+json")
	if err != nil {
		return nil, err
	}
	var raw []struct {
		Body     string `json:"body"`
		HTMLURL  string `json:"html_url"`
		IssueURL string `json:"issue_url"`
		User     struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, err
	}
	var cs []IssueComment
	for _, c := range raw {
		cs = append(cs, IssueComment{
			IssueNumber: numberFromIssueURL(c.IssueURL),
			ExternalID:  c.HTMLURL,
			Author:      c.User.Login,
			Body:        c.Body,
			IsPR:        strings.Contains(c.HTMLURL, "/pull/"),
		})
	}
	return cs, nil
}

// GetIssue fetches a single issue's fields via gh.
func (g *GitForge) GetIssue(ctx context.Context, repo string, number int) (Issue, error) {
	out, err := g.gh(ctx, "issue", "view", strconv.Itoa(number), "--repo", repo,
		"--json", "number,title,body,url,author,assignees")
	if err != nil {
		return Issue{}, err
	}
	return parseIssue(out)
}

// ListAssignedIssues returns the open issues assigned to `assignee` via gh.
func (g *GitForge) ListAssignedIssues(ctx context.Context, repo, assignee string) ([]Issue, error) {
	out, err := g.gh(ctx, "issue", "list", "--repo", repo, "--state", "open",
		"--assignee", assignee, "--limit", "50",
		"--json", "number,title,body,url,author,assignees")
	if err != nil {
		return nil, err
	}
	var raw []issueJSON
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, err
	}
	var out2 []Issue
	for _, r := range raw {
		out2 = append(out2, r.toIssue())
	}
	return out2, nil
}

// LatestAssignment returns who most recently assigned `assignee` to the issue
// and that assignment event's id, by scanning the issue's events for the latest
// "assigned" event whose assignee matches. Events are returned oldest-first, so
// the last match wins; its id lets a later re-assignment (a new event) re-fire
// instead of being deduped against the first. A missing actor yields "" (the
// caller treats that as "cannot authorize yet").
func (g *GitForge) LatestAssignment(ctx context.Context, repo string, number int, assignee string) (string, string, error) {
	out, err := g.gh(ctx, "api",
		"repos/"+repo+"/issues/"+strconv.Itoa(number)+"/events?per_page=100",
		"-H", "Accept: application/vnd.github+json")
	if err != nil {
		return "", "", err
	}
	var raw []struct {
		ID    int64  `json:"id"`
		Event string `json:"event"`
		Actor struct {
			Login string `json:"login"`
		} `json:"actor"`
		Assignee struct {
			Login string `json:"login"`
		} `json:"assignee"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return "", "", err
	}
	actor, eventID := "", ""
	for _, e := range raw {
		if e.Event == "assigned" && strings.EqualFold(e.Assignee.Login, assignee) {
			actor, eventID = e.Actor.Login, strconv.FormatInt(e.ID, 10)
		}
	}
	return actor, eventID, nil
}

// CommentIssue posts an issue comment via gh.
func (g *GitForge) CommentIssue(ctx context.Context, repo string, number int, body string) error {
	_, err := g.gh(ctx, "issue", "comment", strconv.Itoa(number), "--repo", repo, "--body", body)
	return err
}

// issueJSON is the shared shape `gh issue view`/`gh issue list` return.
type issueJSON struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	URL    string `json:"url"`
	Author struct {
		Login string `json:"login"`
	} `json:"author"`
	Assignees []struct {
		Login string `json:"login"`
	} `json:"assignees"`
}

func (r issueJSON) toIssue() Issue {
	iss := Issue{Number: r.Number, Title: r.Title, Body: r.Body, URL: r.URL, Author: r.Author.Login}
	for _, a := range r.Assignees {
		iss.Assignees = append(iss.Assignees, a.Login)
	}
	return iss
}

func parseIssue(out string) (Issue, error) {
	var r issueJSON
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		return Issue{}, err
	}
	return r.toIssue(), nil
}

// numberFromIssueURL extracts N from an API issue_url like
// "https://api.github.com/repos/owner/repo/issues/N".
func numberFromIssueURL(url string) int {
	url = strings.TrimRight(url, "/")
	i := strings.LastIndex(url, "/")
	if i < 0 {
		return 0
	}
	n, _ := strconv.Atoi(url[i+1:])
	return n
}
