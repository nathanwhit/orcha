package notify

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSend_PostsNtfyJSON(t *testing.T) {
	got := make(chan map[string]any, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content-type = %q, want application/json", ct)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		got <- body
	}))
	defer srv.Close()

	New(srv.URL, "my-topic").Send(Notification{
		Title:   "PR opened",
		Message: "opened PR #5",
		Tags:    []string{"rocket"},
	})

	select {
	case body := <-got:
		if body["topic"] != "my-topic" {
			t.Errorf("topic = %v, want my-topic", body["topic"])
		}
		if body["title"] != "PR opened" {
			t.Errorf("title = %v, want PR opened", body["title"])
		}
		if body["message"] != "opened PR #5" {
			t.Errorf("message = %v, want opened PR #5", body["message"])
		}
		tags, _ := body["tags"].([]any)
		if len(tags) != 1 || tags[0] != "rocket" {
			t.Errorf("tags = %v, want [rocket]", body["tags"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no POST received within timeout")
	}
}

func TestSend_OmitsEmptyTopic(t *testing.T) {
	got := make(chan map[string]any, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		got <- body
	}))
	defer srv.Close()

	New(srv.URL, "").Send(Notification{Message: "hi"})

	select {
	case body := <-got:
		if _, ok := body["topic"]; ok {
			t.Errorf("expected topic omitted when empty, got %v", body["topic"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no POST received within timeout")
	}
}

func TestNew_NilWhenUnconfigured(t *testing.T) {
	if New("", "topic") != nil {
		t.Fatal("expected nil notifier when url is empty")
	}
	// A nil notifier must be safe to call: the orchestrator sends unconditionally.
	var n *Notifier
	n.Send(Notification{Message: "must not panic"})
}
