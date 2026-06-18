package orch

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nathanwhit/orcha/internal/notify"
)

// TestNotifyEvent_AllowlistAndPayload pins the two things that matter about the
// audit->notify bridge: only allowlisted event types page the user, and the
// payload carries the spec's title plus the event summary as the message.
func TestNotifyEvent_AllowlistAndPayload(t *testing.T) {
	o, _ := newTestOrch(t)

	posts := make(chan map[string]any, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		posts <- b
	}))
	defer srv.Close()
	o.SetNotifier(notify.New(srv.URL, "t"))

	// An allowlisted event is forwarded with its category title and summary.
	o.notifyEvent("ask_user", "what color should the button be?", nil)
	select {
	case b := <-posts:
		if b["title"] != "Needs your input" {
			t.Errorf("title = %v, want Needs your input", b["title"])
		}
		if b["message"] != "what color should the button be?" {
			t.Errorf("message = %v, want the question text", b["message"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected a notification for ask_user")
	}

	// A high-churn event that is not on the allowlist stays silent.
	o.notifyEvent("usage_synced", "claude 28%", nil)
	select {
	case b := <-posts:
		t.Fatalf("did not expect a notification for usage_synced, got %v", b)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestNotifyEvent_NoNotifier is the default path: without a configured notifier,
// notifyEvent is a no-op and must not panic.
func TestNotifyEvent_NoNotifier(t *testing.T) {
	o, _ := newTestOrch(t)
	o.notifyEvent("ask_user", "anyone home?", nil)
}
