package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nathanwhit/orcha/internal/agent"
	"github.com/nathanwhit/orcha/internal/forge"
	"github.com/nathanwhit/orcha/internal/model"
	"github.com/nathanwhit/orcha/internal/orch"
	"github.com/nathanwhit/orcha/internal/store"
)

func newTestServer(t *testing.T) (*httptest.Server, *orch.Orchestrator, *store.Store) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	o := orch.New(st, orch.Config{Guards: orch.DefaultGuards()})
	o.RegisterProvider(agent.NewFake(model.AgentClaude, true, nil))
	o.SetForge(forge.NewFake())
	srv := httptest.NewServer(New(o).Handler())
	t.Cleanup(func() { srv.Close(); st.Close() })
	return srv, o, st
}

func getJSON(t *testing.T, url string, into any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
}

func postJSON(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

// The dashboard endpoint must stay small even with large transcripts/logs.
func TestDashboard_StaysSmallWithManyLogs(t *testing.T) {
	srv, o, st := newTestServer(t)

	_, mgr, err := o.CreateObjective(orch.NewObjectiveSpec{Title: "Heavy", Prompt: "p", Agent: model.AgentClaude})
	if err != nil {
		t.Fatalf("create objective: %v", err)
	}
	huge := strings.Repeat("Z", 100_000)
	for i := 0; i < 100; i++ {
		_ = st.AppendMessage(&model.Message{SessionID: mgr.ID, Source: model.MsgStdout, Kind: model.KindText, Content: huge})
	}

	resp, err := http.Get(srv.URL + "/api/objectives")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	n, _ := buf.ReadFrom(resp.Body)

	// ~10MB of transcript exists; the dashboard payload must be a tiny fraction
	// of that and contain none of the blob.
	if n > 16*1024 {
		t.Fatalf("dashboard payload too large: %d bytes (transcript leaked?)", n)
	}
	if strings.Contains(buf.String(), huge) {
		t.Fatal("dashboard leaked transcript content")
	}
}

func TestQuestionAnswerFlow_UpdatesObjective(t *testing.T) {
	srv, o, st := newTestServer(t)
	obj, mgr, _ := o.CreateObjective(orch.NewObjectiveSpec{Title: "x", Prompt: "p", Agent: model.AgentClaude})

	// Manager asks the user a question -> objective goes waiting_user.
	q, err := o.AskUser(mgr.ID, "Which database?", "need a choice", 5)
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	reloaded, _ := st.GetObjective(obj.ID)
	if reloaded.Status != model.ObjectiveWaitingUser {
		t.Fatalf("objective should be waiting_user, got %s", reloaded.Status)
	}

	// It appears in the needs-user queue.
	var open []model.Question
	getJSON(t, srv.URL+"/api/questions", &open)
	if len(open) != 1 || open[0].ID != q.ID {
		t.Fatalf("question not in needs-user queue: %+v", open)
	}

	// Answer it via the API -> question answered, objective back to active.
	resp := postJSON(t, srv.URL+"/api/questions/"+q.ID+"/answer", map[string]string{"answer": "postgres"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("answer status %d", resp.StatusCode)
	}
	after, _ := st.GetObjective(obj.ID)
	if after.Status != model.ObjectiveActive {
		t.Fatalf("objective should return to active, got %s", after.Status)
	}
	// The answer is delivered into the manager session transcript.
	msgs, _ := st.MessagesAfter(mgr.ID, 0, 100)
	found := false
	for _, m := range msgs {
		if strings.Contains(m.Content, "postgres") {
			found = true
		}
	}
	if !found {
		t.Fatal("answer should be delivered to the session transcript")
	}
}

func TestTargetStatus_AppearsAndDrains(t *testing.T) {
	srv, _, st := newTestServer(t)
	tgt := &model.Target{Name: "remote-1", Kind: model.TargetSSH, Status: model.TargetOnline,
		WorkRoot: "/home/bot/work", CapacitySessions: 3, Host: "h", User: "bot"}
	_ = st.CreateTarget(tgt)

	var targets []model.Target
	getJSON(t, srv.URL+"/api/targets", &targets)
	if len(targets) != 1 || targets[0].Name != "remote-1" || targets[0].Status != model.TargetOnline {
		t.Fatalf("target should appear online in dashboard: %+v", targets)
	}

	resp := postJSON(t, srv.URL+"/api/targets/"+tgt.ID+"/drain", nil)
	resp.Body.Close()
	drained, _ := st.GetTarget(tgt.ID)
	if drained.Status != model.TargetDraining {
		t.Fatalf("target should be draining, got %s", drained.Status)
	}
}

func TestMessagesEndpoint_Incremental(t *testing.T) {
	srv, o, st := newTestServer(t)
	_, mgr, _ := o.CreateObjective(orch.NewObjectiveSpec{Title: "x", Prompt: "p", Agent: model.AgentClaude})
	for i := 0; i < 3; i++ {
		_ = st.AppendMessage(&model.Message{SessionID: mgr.ID, Source: model.MsgAgent, Kind: model.KindText, Content: "row"})
	}
	var msgs []model.Message
	getJSON(t, srv.URL+"/api/sessions/"+mgr.ID+"/messages?after=1", &msgs)
	if len(msgs) != 2 || msgs[0].Seq != 2 {
		t.Fatalf("incremental fetch wrong: %+v", msgs)
	}
}

func TestSessionScreen_NoLiveScreenReturns204(t *testing.T) {
	srv, o, _ := newTestServer(t)
	_, mgr, _ := o.CreateObjective(orch.NewObjectiveSpec{Title: "x", Prompt: "p", Agent: model.AgentClaude})
	// The session isn't running (no tmux), so there is no live screen.
	resp, err := http.Get(srv.URL + "/api/sessions/" + mgr.ID + "/screen")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status=%d, want 204", resp.StatusCode)
	}
}
