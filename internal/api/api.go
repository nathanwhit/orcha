// Package api exposes the orchestrator over HTTP. Dashboard endpoints return
// only small rows; transcripts load separately and incrementally via the
// messages and stream endpoints.
package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/nathanwhit/orcha/internal/model"
	"github.com/nathanwhit/orcha/internal/orch"
	"github.com/nathanwhit/orcha/internal/store"
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

	mux.HandleFunc("GET /api/targets", s.listTargets)
	mux.HandleFunc("GET /api/targets/{id}", s.getTarget)
	mux.HandleFunc("POST /api/targets/{id}/drain", s.targetMode(model.TargetDraining))
	mux.HandleFunc("POST /api/targets/{id}/enable", s.targetMode(model.TargetOnline))
	mux.HandleFunc("POST /api/targets/{id}/disable", s.targetMode(model.TargetDisabled))

	mux.HandleFunc("GET /api/pull-requests", s.listPRs)
	mux.HandleFunc("GET /api/pull-requests/{id}", s.getPR)
	mux.HandleFunc("POST /api/pull-requests/{id}/refresh", s.refreshPR)
	mux.HandleFunc("POST /api/pull-requests/{id}/steer", s.steerPR)

	mux.HandleFunc("GET /api/questions", s.listQuestions)
	mux.HandleFunc("POST /api/questions/{id}/answer", s.answerQuestion)

	mux.HandleFunc("GET /api/usage", s.listUsage)
	mux.HandleFunc("GET /api/events", s.listEvents)

	return mux
}

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
	writeJSON(w, http.StatusOK, rows)
}

type createObjectiveReq struct {
	Title  string `json:"title"`
	Prompt string `json:"prompt"`
	Agent  string `json:"agent"`
}

func (s *Server) createObjective(w http.ResponseWriter, r *http.Request) {
	var req createObjectiveReq
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	obj, mgr, err := s.o.CreateObjective(orch.NewObjectiveSpec{
		Title: req.Title, Prompt: req.Prompt, Agent: model.AgentKind(req.Agent),
	})
	if err != nil {
		writeErr(w, httpStatusFor(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"objective": obj, "manager": mgr})
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
	writeJSON(w, http.StatusOK, map[string]any{
		"objective":     obj,
		"sessions":      sessions,
		"pull_requests": prs,
		"questions":     questions,
		"artifacts":     artifacts,
	})
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
	writeJSON(w, http.StatusOK, rows)
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
	writeJSON(w, http.StatusOK, msgs)
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
	writeJSON(w, http.StatusOK, targets)
}

func (s *Server) getTarget(w http.ResponseWriter, r *http.Request) {
	t, err := s.st.GetTarget(r.PathValue("id"))
	if err != nil {
		writeErr(w, httpStatusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, t)
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
	writeJSON(w, http.StatusOK, prs)
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
	writeJSON(w, http.StatusOK, qs)
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
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	events, err := s.st.EventsAfter(after, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, events)
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
