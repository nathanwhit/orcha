// Package api exposes the orchestrator over HTTP. Dashboard endpoints return
// only small rows; transcripts load separately and incrementally via the
// messages and stream endpoints.
package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/nathanwhit/orcha/internal/model"
	"github.com/nathanwhit/orcha/internal/orch"
	"github.com/nathanwhit/orcha/internal/store"
	"github.com/nathanwhit/orcha/internal/version"
)

// Server is the HTTP API.
type Server struct {
	o  *orch.Orchestrator
	st *store.Store
}

// New builds an API server.
func New(o *orch.Orchestrator) *Server {
	return &Server{o: o, st: o.Store()}
}

// Handler returns the configured HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/objectives", s.listObjectives)
	mux.HandleFunc("POST /api/objectives", s.createObjective)
	mux.HandleFunc("GET /api/objectives/{id}", s.getObjective)
	mux.HandleFunc("POST /api/objectives/{id}/steer", s.steerObjective)
	mux.HandleFunc("POST /api/objectives/{id}/cancel", s.cancelObjective)

	mux.HandleFunc("GET /api/sessions", s.listSessions)
	mux.HandleFunc("GET /api/sessions/{id}", s.getSession)
	mux.HandleFunc("GET /api/sessions/{id}/messages", s.sessionMessages)
	mux.HandleFunc("POST /api/sessions/{id}/messages", s.postSessionMessage)
	mux.HandleFunc("POST /api/sessions/{id}/cancel", s.cancelSession)
	mux.HandleFunc("POST /api/sessions/{id}/restart", s.restartSession)
	mux.HandleFunc("GET /api/sessions/{id}/stream", s.streamSession)
	mux.HandleFunc("GET /api/sessions/{id}/screen", s.sessionScreen)

	mux.HandleFunc("GET /api/targets", s.listTargets)
	mux.HandleFunc("POST /api/targets", s.createTarget)
	mux.HandleFunc("GET /api/targets/{id}", s.getTarget)
	mux.HandleFunc("POST /api/targets/{id}/drain", s.targetMode(model.TargetDraining))
	mux.HandleFunc("POST /api/targets/{id}/enable", s.targetMode(model.TargetOnline))
	mux.HandleFunc("POST /api/targets/{id}/disable", s.targetMode(model.TargetDisabled))
	mux.HandleFunc("POST /api/targets/{id}/healthcheck", s.healthcheckTarget)
	mux.HandleFunc("POST /api/targets/{id}/doctor", s.doctorTarget)

	mux.HandleFunc("GET /api/pull-requests", s.listPRs)
	mux.HandleFunc("GET /api/pull-requests/{id}", s.getPR)
	mux.HandleFunc("POST /api/pull-requests/{id}/refresh", s.refreshPR)
	mux.HandleFunc("POST /api/pull-requests/{id}/steer", s.steerPR)
	mux.HandleFunc("POST /api/pull-requests/{id}/sync", s.syncPR)

	mux.HandleFunc("GET /api/projects", s.listProjects)
	mux.HandleFunc("POST /api/projects", s.upsertProject)
	mux.HandleFunc("DELETE /api/projects/{id}", s.deleteProject)

	mux.HandleFunc("GET /api/questions", s.listQuestions)
	mux.HandleFunc("POST /api/questions/{id}/answer", s.answerQuestion)

	mux.HandleFunc("GET /api/usage", s.listUsage)
	mux.HandleFunc("GET /api/events", s.listEvents)

	mux.HandleFunc("GET /api/health", s.health)

	return mux
}

// ---- health ----

// health is a lightweight liveness/version probe for monitoring and the
// dashboard. It also reports the current server time (RFC3339) so callers can
// detect clock skew, plus the VCS revision and process start time — so "which
// build is actually running?" is answerable from the endpoint instead of
// guessed (a stale process looks exactly like a broken fix otherwise).
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"version": version.Version,
		"build":   buildRevision,
		"started": processStart.UTC().Format(time.RFC3339),
		"time":    time.Now().UTC().Format(time.RFC3339),
	})
}

var processStart = time.Now()

// buildRevision is the VCS revision stamped into the binary by `go build`.
// `go run` does not stamp VCS info, so it reports "unstamped".
var buildRevision = func() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	rev, dirty := "", ""
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "+dirty"
			}
		}
	}
	if rev == "" {
		return "unstamped"
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	return rev + dirty
}()

// ---- helpers ----

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func decode(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

func httpStatusFor(err error) int {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, store.ErrNoCapacity), errors.Is(err, orch.ErrNoTarget),
		errors.Is(err, store.ErrLockHeld), errors.Is(err, orch.ErrUnsafePublish):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

// ---- objectives ----

func (s *Server) listObjectives(w http.ResponseWriter, r *http.Request) {
	// Dashboard rows only: small scalars + counts, never transcript content.
	rows, err := s.st.DashboardObjectives()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, orEmpty(rows))
}

type createObjectiveReq struct {
	Title      string `json:"title"`
	Prompt     string `json:"prompt"`
	Agent      string `json:"agent"`
	ProjectID  string `json:"project_id"` // registered project; explicit fields below override
	Repo       string `json:"repo"`
	PushRepo   string `json:"push_repo"`
	BaseBranch string `json:"base_branch"`
}

func (s *Server) createObjective(w http.ResponseWriter, r *http.Request) {
	var req createObjectiveReq
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// A registered project supplies repo/fork/base defaults; explicit fields win.
	if req.ProjectID != "" {
		p, err := s.st.GetProject(req.ProjectID)
		if err != nil {
			writeErr(w, httpStatusFor(err), err)
			return
		}
		if req.Repo == "" {
			req.Repo = p.Repo
		}
		if req.PushRepo == "" {
			req.PushRepo = p.PushRepo
		}
		if req.BaseBranch == "" {
			req.BaseBranch = p.BaseBranch
		}
	}
	obj, mgr, err := s.o.CreateObjective(orch.NewObjectiveSpec{
		Title: req.Title, Prompt: req.Prompt, Agent: model.AgentKind(req.Agent),
		Repo: req.Repo, PushRepo: req.PushRepo, BaseBranch: req.BaseBranch,
	})
	if err != nil {
		writeErr(w, httpStatusFor(err), err)
		return
	}
	// Remember a typed repo as a project so next time it's a pick, not typing.
	if req.ProjectID == "" && req.Repo != "" {
		_ = s.st.UpsertProject(&model.Project{
			Repo: req.Repo, PushRepo: req.PushRepo, BaseBranch: req.BaseBranch,
		})
	}
	writeJSON(w, http.StatusCreated, map[string]any{"objective": obj, "manager": mgr})
}

// ---- projects ----

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	ps, err := s.st.ListProjects()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, orEmpty(ps))
}

type upsertProjectReq struct {
	Name       string `json:"name"`
	Repo       string `json:"repo"`
	PushRepo   string `json:"push_repo"`
	BaseBranch string `json:"base_branch"`
	CloneURL   string `json:"clone_url"`
}

func (s *Server) upsertProject(w http.ResponseWriter, r *http.Request) {
	var req upsertProjectReq
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Repo == "" {
		writeErr(w, http.StatusBadRequest, errors.New("repo is required (owner/repo)"))
		return
	}
	p := &model.Project{
		Name: req.Name, Repo: req.Repo, PushRepo: req.PushRepo,
		BaseBranch: req.BaseBranch, CloneURL: req.CloneURL,
	}
	if err := s.st.UpsertProject(p); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (s *Server) deleteProject(w http.ResponseWriter, r *http.Request) {
	if err := s.st.DeleteProject(r.PathValue("id")); err != nil {
		writeErr(w, httpStatusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) getObjective(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	obj, err := s.st.GetObjective(id)
	if err != nil {
		writeErr(w, httpStatusFor(err), err)
		return
	}
	sessions, _ := s.st.DashboardSessions(id)
	prs, _ := s.st.ListPRsByObjective(id)
	questions, _ := s.st.ListQuestionsByObjective(id)
	artifacts, _ := s.st.ListArtifactsByObjective(id)
	// Lists are never null in the JSON contract (a Go nil slice encodes as
	// null, which clients then call array methods on).
	writeJSON(w, http.StatusOK, map[string]any{
		"objective":     obj,
		"sessions":      orEmpty(sessions),
		"pull_requests": orEmpty(prs),
		"questions":     orEmpty(questions),
		"artifacts":     orEmpty(artifacts),
	})
}

// orEmpty coalesces a nil slice to an empty one so it encodes as [] not null.
func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

type steerReq struct {
	Message string `json:"message"`
}

func (s *Server) steerObjective(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req steerReq
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	obj, err := s.st.GetObjective(id)
	if err != nil {
		writeErr(w, httpStatusFor(err), err)
		return
	}
	if obj.ManagerSessionID == "" {
		writeErr(w, http.StatusConflict, errors.New("objective has no manager session"))
		return
	}
	if err := s.o.Steer(r.Context(), obj.ManagerSessionID, req.Message); err != nil {
		writeErr(w, httpStatusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "steered"})
}

func (s *Server) cancelObjective(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.o.CancelObjective(id, "canceled via API"); err != nil {
		writeErr(w, httpStatusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "canceled"})
}

// ---- sessions ----

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	rows, err := s.st.DashboardSessions(r.URL.Query().Get("objective_id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, orEmpty(rows))
}

func (s *Server) getSession(w http.ResponseWriter, r *http.Request) {
	sess, err := s.st.GetSession(r.PathValue("id"))
	if err != nil {
		writeErr(w, httpStatusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

func (s *Server) sessionMessages(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	msgs, err := s.st.MessagesAfter(id, after, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, orEmpty(msgs))
}

func (s *Server) postSessionMessage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req steerReq
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.o.Steer(r.Context(), id, req.Message); err != nil {
		writeErr(w, httpStatusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "sent"})
}

// sessionScreen returns the live terminal screen (e.g. tmux capture-pane) for a
// running session, for the dashboard's terminal panel. 204 when there is no live
// screen (session not running, or a non-terminal provider).
func (s *Server) sessionScreen(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	screen, ok, err := s.o.SessionScreen(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	sess, _ := s.st.GetSession(id)
	attach := ""
	if sess != nil {
		attach, _ = sess.Metadata["tmux_attach"].(string)
	}
	writeJSON(w, http.StatusOK, map[string]any{"screen": screen, "attach": attach})
}

func (s *Server) cancelSession(w http.ResponseWriter, r *http.Request) {
	if err := s.o.Cancel(r.PathValue("id"), true); err != nil {
		writeErr(w, httpStatusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "canceled"})
}

func (s *Server) restartSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	run, err := s.o.StartRun(r.Context(), id)
	if err != nil {
		writeErr(w, httpStatusFor(err), err)
		return
	}
	_ = run
	writeJSON(w, http.StatusOK, map[string]string{"status": "restarted"})
}

// ---- targets ----

func (s *Server) listTargets(w http.ResponseWriter, r *http.Request) {
	targets, err := s.st.ListTargets()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, orEmpty(targets))
}

func (s *Server) getTarget(w http.ResponseWriter, r *http.Request) {
	t, err := s.st.GetTarget(r.PathValue("id"))
	if err != nil {
		writeErr(w, httpStatusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

type createTargetReq struct {
	Name             string   `json:"name"`
	Kind             string   `json:"kind"` // local | ssh
	Host             string   `json:"host"`
	User             string   `json:"user"`
	WorkRoot         string   `json:"work_root"`
	Labels           []string `json:"labels"`
	CapacitySessions int      `json:"capacity_sessions"`
	SSHPort          int      `json:"ssh_port"`
	IdentityFile     string   `json:"identity_file"`
	Bootstrap        string   `json:"bootstrap"`
}

// createTarget registers a machine (local or SSH). For SSH it health-checks the
// host, so the response status reflects whether it is reachable (this can take a
// few seconds for an unreachable host).
func (s *Server) createTarget(w http.ResponseWriter, r *http.Request) {
	var req createTargetReq
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	kind := model.TargetKind(req.Kind)
	if kind == "" {
		kind = model.TargetSSH
	}
	meta := model.JSONMap{}
	if req.SSHPort != 0 {
		meta["ssh_port"] = float64(req.SSHPort)
	}
	if req.IdentityFile != "" {
		meta["identity_file"] = req.IdentityFile
	}
	if req.Bootstrap != "" {
		meta["bootstrap"] = req.Bootstrap
	}
	t := &model.Target{
		Name: req.Name, Kind: kind, Host: req.Host, User: req.User,
		WorkRoot: req.WorkRoot, Labels: req.Labels, CapacitySessions: req.CapacitySessions,
		Metadata: meta,
	}
	created, doctor, err := s.o.RegisterTarget(r.Context(), t)
	if err != nil {
		writeErr(w, httpStatusFor(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"target": created, "doctor": doctor})
}

// doctorTarget re-runs readiness diagnostics on a target.
func (s *Server) doctorTarget(w http.ResponseWriter, r *http.Request) {
	rep, err := s.o.DoctorTarget(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, httpStatusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

func (s *Server) healthcheckTarget(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := s.o.HealthCheckTarget(r.Context(), id)
	t, gerr := s.st.GetTarget(id)
	if gerr != nil {
		writeErr(w, httpStatusFor(gerr), gerr)
		return
	}
	resp := map[string]any{"target": t}
	if err != nil {
		resp["error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) targetMode(status model.TargetStatus) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := s.st.SetTargetStatus(r.PathValue("id"), status); err != nil {
			writeErr(w, httpStatusFor(err), err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": string(status)})
	}
}

// ---- pull requests ----

func (s *Server) listPRs(w http.ResponseWriter, r *http.Request) {
	prs, err := s.st.ListPRs()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, orEmpty(prs))
}

func (s *Server) getPR(w http.ResponseWriter, r *http.Request) {
	pr, err := s.st.GetPR(r.PathValue("id"))
	if err != nil {
		writeErr(w, httpStatusFor(err), err)
		return
	}
	fb, _ := s.st.UnhandledFeedback(pr.ID)
	writeJSON(w, http.StatusOK, map[string]any{"pull_request": pr, "pending_feedback": fb})
}

func (s *Server) refreshPR(w http.ResponseWriter, r *http.Request) {
	pr, err := s.o.RefreshPR(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, httpStatusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, pr)
}

// syncPR polls the host for new comments and spawns follow-up sessions for
// actionable ones.
func (s *Server) syncPR(w http.ResponseWriter, r *http.Request) {
	spawned, err := s.o.SyncPRFeedback(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, httpStatusFor(err), err)
		return
	}
	ids := make([]string, 0, len(spawned))
	for _, sess := range spawned {
		ids = append(ids, sess.ID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"followups_spawned": ids})
}

func (s *Server) steerPR(w http.ResponseWriter, r *http.Request) {
	// Steering a PR routes to its newest attached follow-up session.
	id := r.PathValue("id")
	var req steerReq
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	pr, err := s.st.GetPR(id)
	if err != nil {
		writeErr(w, httpStatusFor(err), err)
		return
	}
	sessions, _ := s.st.ListSessionsByObjective(pr.ObjectiveID)
	var target *model.Session
	for _, sess := range sessions {
		if sess.Metadata["pr_id"] == id && !sess.Status.IsTerminal() {
			target = sess
		}
	}
	if target == nil {
		writeErr(w, http.StatusConflict, errors.New("no active follow-up session for PR"))
		return
	}
	if err := s.o.Steer(r.Context(), target.ID, req.Message); err != nil {
		writeErr(w, httpStatusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "steered", "session_id": target.ID})
}

// ---- questions / usage / events ----

func (s *Server) listQuestions(w http.ResponseWriter, r *http.Request) {
	qs, err := s.st.ListOpenQuestions()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, orEmpty(qs))
}

type answerReq struct {
	Answer string `json:"answer"`
}

func (s *Server) answerQuestion(w http.ResponseWriter, r *http.Request) {
	var req answerReq
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	q, err := s.o.AnswerQuestion(r.PathValue("id"), req.Answer)
	if err != nil {
		writeErr(w, httpStatusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, q)
}

func (s *Server) listUsage(w http.ResponseWriter, r *http.Request) {
	u, err := s.st.ListUsage()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, orEmpty(u))
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	events, err := s.st.EventsAfter(after, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, orEmpty(events))
}

// streamSession streams transcript rows incrementally as Server-Sent Events.
// It polls the message table by seq cursor — never loading the whole transcript
// at once.
func (s *Server) streamSession(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, errors.New("streaming unsupported"))
		return
	}
	id := r.PathValue("id")
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	ctx := r.Context()
	for {
		msgs, err := s.st.MessagesAfter(id, after, 200)
		if err != nil {
			return
		}
		for _, m := range msgs {
			after = m.Seq
			b, _ := json.Marshal(m)
			_, _ = w.Write([]byte("data: "))
			_, _ = w.Write(b)
			_, _ = w.Write([]byte("\n\n"))
		}
		flusher.Flush()

		// Stop streaming once the session is terminal and fully drained.
		sess, err := s.st.GetSession(id)
		if err != nil || (sess.Status.IsTerminal()) {
			latest, _ := s.st.LatestSeq(id)
			if latest <= after {
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
