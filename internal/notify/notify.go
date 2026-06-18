// Package notify delivers high-signal orchestrator events to an external
// endpoint as a fire-and-forget JSON POST. The body matches ntfy's JSON publish
// format (https://docs.ntfy.sh/publish/#publish-as-json) so pointing -notify-url
// at an ntfy server "just works" for phone/desktop push — but it is only a plain
// JSON POST, so any endpoint that accepts {topic,title,message,tags} works too.
//
// Best-effort by design: a notification must never block or fail the work that
// triggered it. Sends run on their own goroutine with a short client timeout and
// log (rather than return) errors, and an unconfigured Notifier is nil and no-ops
// so callers never have to branch on configuration.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"
)

// Notification is one outbound message. Message is required; Title and Tags are
// optional. Tags are ntfy emoji shortcodes (e.g. "rocket") and are pure
// decoration that other receivers can ignore.
type Notification struct {
	Title   string
	Message string
	Tags    []string
}

// Notifier posts notifications to a configured endpoint.
type Notifier struct {
	url    string
	topic  string
	client *http.Client
}

// New returns a Notifier that POSTs to url, or nil when url is empty (an
// unconfigured, no-op notifier). topic is the ntfy topic, carried in the JSON
// body; leave it empty for endpoints that don't use topics.
func New(url, topic string) *Notifier {
	if url == "" {
		return nil
	}
	return &Notifier{
		url:    url,
		topic:  topic,
		client: &http.Client{Timeout: 8 * time.Second},
	}
}

// Send delivers n in the background and returns immediately. A nil Notifier
// drops the call silently, so an unconfigured orchestrator can call Send freely.
func (n *Notifier) Send(note Notification) {
	if n == nil {
		return
	}
	go n.post(note)
}

// wirePayload is the JSON shape ntfy expects on its root endpoint.
type wirePayload struct {
	Topic   string   `json:"topic,omitempty"`
	Title   string   `json:"title,omitempty"`
	Message string   `json:"message"`
	Tags    []string `json:"tags,omitempty"`
}

func (n *Notifier) post(note Notification) {
	body, err := json.Marshal(wirePayload{
		Topic:   n.topic,
		Title:   note.Title,
		Message: note.Message,
		Tags:    note.Tags,
	})
	if err != nil {
		log.Printf("notify: marshal: %v", err)
		return
	}
	// The triggering call (an audit, often under a turn that ends right after)
	// must not own this request's lifetime, so use a fresh bounded context; the
	// client timeout is the real backstop against a hung endpoint.
	ctx, cancel := context.WithTimeout(context.Background(), n.client.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.url, bytes.NewReader(body))
	if err != nil {
		log.Printf("notify: build request: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		log.Printf("notify: post: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		log.Printf("notify: %s returned %s", n.url, resp.Status)
	}
}
