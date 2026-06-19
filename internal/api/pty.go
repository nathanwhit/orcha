package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/coder/websocket"
)

// ptyClientMsg is a control or data frame from the browser terminal. A text
// frame carrying {"type":"resize",...} resizes the pty; any other frame's bytes
// are written to the pty as keystrokes.
type ptyClientMsg struct {
	Type string `json:"type"`
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

// sessionPTY upgrades to a WebSocket and bridges it to a live, interactive pty
// attached to the session's tmux pane: pty output streams to the browser as
// binary frames, and browser frames are written back as input. This is a remote
// shell — it inherits whatever access control fronts the dashboard, nothing more.
func (s *Server) sessionPTY(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cols := uint16(atoiDefault(r.URL.Query().Get("cols"), 80))
	rows := uint16(atoiDefault(r.URL.Query().Get("rows"), 24))

	// Cross-site WebSocket hijacking guard: the built-in origin check compares
	// against r.Host, which behind the exe.dev proxy is the internal host, not
	// the public origin — so check against X-Forwarded-Host ourselves and tell
	// the library to skip its own (OriginPatterns "*").
	if !sameOrigin(r) {
		http.Error(w, "forbidden: bad origin", http.StatusForbidden)
		return
	}

	proc, ok, err := s.o.SessionAttach(id, cols, rows)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if !ok {
		// Not running, or a provider with no live terminal.
		w.WriteHeader(http.StatusNotFound)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
		OriginPatterns:  []string{"*"}, // origin already verified via sameOrigin
	})
	if err != nil {
		_ = proc.Close()
		return
	}
	// 1 MiB is plenty for a paste; terminal input is otherwise tiny.
	conn.SetReadLimit(1 << 20)

	// A cancelable context ties both pump directions together: whichever side
	// ends first cancels the other, then we close the pty and the socket.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	defer proc.Close()

	// pty -> browser
	go func() {
		defer cancel()
		buf := make([]byte, 32*1024)
		for {
			n, rerr := proc.Read(buf)
			if n > 0 {
				if werr := conn.Write(ctx, websocket.MessageBinary, buf[:n]); werr != nil {
					return
				}
			}
			if rerr != nil {
				return
			}
		}
	}()

	// browser -> pty. Keystrokes arrive as binary frames (written verbatim);
	// control frames (resize) arrive as text JSON. Keeping them on separate
	// frame types avoids misreading a typed "{" as a control message.
	for {
		typ, data, rerr := conn.Read(ctx)
		if rerr != nil {
			break
		}
		if typ == websocket.MessageText {
			var m ptyClientMsg
			if json.Unmarshal(data, &m) == nil && m.Type == "resize" && m.Cols > 0 && m.Rows > 0 {
				_ = proc.Resize(m.Cols, m.Rows)
			}
			continue
		}
		if _, werr := proc.Write(data); werr != nil {
			break
		}
	}

	conn.Close(websocket.StatusNormalClosure, "")
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return def
	}
	return n
}
