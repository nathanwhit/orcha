import { useEffect, useRef, useState } from "react";
import * as api from "../api";
import { usePoll } from "../hooks";
import { Icon } from "../icons";
import {
  Button,
  Card,
  Chip,
  CopyButton,
  Pill,
  SectionTitle,
  SteerBox,
  TimeAgo,
} from "../ui";

export function SessionPage({ id }: { id: string }) {
  const session = usePoll(
    () => api.get<api.Session>(`/api/sessions/${id}`),
    2000,
    [id],
  );
  const sess = session.data;
  const terminalStatus =
    sess != null && ["succeeded", "failed", "canceled"].includes(sess.status);

  if (session.error && !sess)
    return <p className="text-sm text-rose-400">{session.error}</p>;
  if (!sess) return <p className="text-sm text-faint">Loading…</p>;

  return (
    <div className="space-y-5">
      <div>
        <a
          href={sess.objective_id ? `#/objectives/${sess.objective_id}` : "#/sessions"}
          className="mb-3 inline-flex items-center gap-1 text-xs text-mute transition-colors hover:text-ink"
        >
          <Icon name="back" className="size-3.5" />
          {sess.objective_id ? "Objective" : "Sessions"}
        </a>
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div className="min-w-0">
            <div className="flex flex-wrap items-center gap-3">
              <h1 className="text-xl font-semibold tracking-tight">
                {sess.title || "(untitled session)"}
              </h1>
              <Pill status={sess.status} />
            </div>
            <p className="mt-1.5 flex flex-wrap items-center gap-2 text-xs text-faint">
              <span className="font-mono">{api.shortId(sess.id)}</span>
              <Chip>{sess.role.replaceAll("_", " ")}</Chip>
              <Chip>{sess.agent}</Chip>
              <Chip>{sess.mode}</Chip>
              {sess.current_activity && <span>{sess.current_activity}</span>}
            </p>
          </div>
          {!terminalStatus && (
            <Button
              variant="danger"
              onClick={() => {
                if (confirm("Cancel this session?"))
                  void api
                    .post(`/api/sessions/${id}/cancel`)
                    .then(session.reload);
              }}
            >
              <Icon name="stop" className="size-3.5" />
              Cancel
            </Button>
          )}
        </div>
      </div>

      {sess.goal && (
        <Card className="px-4 py-3">
          <p className="text-[11px] font-semibold tracking-wider text-faint uppercase">
            Goal
          </p>
          <p className="mt-1 line-clamp-6 text-sm whitespace-pre-wrap text-mute">
            {sess.goal}
          </p>
        </Card>
      )}

      <div className="grid gap-5 xl:grid-cols-2">
        <section className="min-w-0">
          <SectionTitle>Transcript</SectionTitle>
          <Transcript id={id} />
          {!terminalStatus && (
            <div className="mt-3">
              <SteerBox
                placeholder="Send a message to this session…"
                onSend={(message) =>
                  api.post(`/api/sessions/${id}/messages`, { message })
                }
              />
            </div>
          )}
        </section>
        <section className="min-w-0">
          <SectionTitle>Live terminal</SectionTitle>
          <TerminalPanel id={id} active={!terminalStatus} />
          {sess.latest_summary && (
            <Card className="mt-4 px-4 py-3">
              <p className="text-[11px] font-semibold tracking-wider text-faint uppercase">
                Latest summary
              </p>
              <p className="mt-1 text-sm whitespace-pre-wrap text-mute">
                {sess.latest_summary}
              </p>
            </Card>
          )}
        </section>
      </div>
    </div>
  );
}

// Transcript incrementally polls messages by seq cursor — it never refetches
// what it already has, mirroring how the API is meant to be consumed. Only the
// most recent messages are kept; a long-running session's history would
// otherwise grow the DOM without bound.
const KEEP_MESSAGES = 1500;

function Transcript({ id }: { id: string }) {
  const [msgs, setMsgs] = useState<api.Message[]>([]);
  const boxRef = useRef<HTMLDivElement>(null);
  const pinnedRef = useRef(true); // stick to bottom unless the user scrolls up

  useEffect(() => {
    setMsgs([]);
    let live = true;
    let after = 0;
    let timer: number | undefined;
    const tick = async () => {
      try {
        const batch = await api.get<api.Message[]>(
          `/api/sessions/${id}/messages?after=${after}&limit=500`,
        );
        if (!live) return;
        if (batch && batch.length > 0) {
          after = batch[batch.length - 1].seq;
          setMsgs((m) => [...m, ...batch].slice(-KEEP_MESSAGES));
        }
      } catch {
        // transient; retry on the next tick
      }
      if (live) timer = window.setTimeout(tick, 1000);
    };
    void tick();
    return () => {
      live = false;
      if (timer !== undefined) clearTimeout(timer);
    };
  }, [id]);

  useEffect(() => {
    const box = boxRef.current;
    if (box && pinnedRef.current) box.scrollTop = box.scrollHeight;
  }, [msgs]);

  const rows = groupMessages(msgs);

  return (
    <Card>
      <div
        ref={boxRef}
        onScroll={(e) => {
          const el = e.currentTarget;
          pinnedRef.current =
            el.scrollHeight - el.scrollTop - el.clientHeight < 60;
        }}
        className="flex h-[28rem] flex-col gap-2.5 overflow-y-auto p-4"
      >
        {rows.length === 0 && (
          <p className="m-auto text-sm text-faint">No messages yet.</p>
        )}
        {rows.map((row) =>
          row.kind === "log" ? (
            <LogBlock key={row.key} lines={row.lines} source={row.source} />
          ) : (
            <MessageRow key={row.key} m={row.m} />
          ),
        )}
      </div>
    </Card>
  );
}

// groupMessages folds runs of consecutive raw stdout/stderr lines into a
// single log block — they're process output, not conversation.
type TranscriptRow =
  | { kind: "msg"; key: number; m: api.Message }
  | { kind: "log"; key: number; source: string; lines: string[] };

function groupMessages(msgs: api.Message[]): TranscriptRow[] {
  const rows: TranscriptRow[] = [];
  for (const m of msgs) {
    if (m.source === "stdout" || m.source === "stderr") {
      const last = rows[rows.length - 1];
      if (last && last.kind === "log" && last.source === m.source) {
        last.lines.push(m.content);
      } else {
        rows.push({ kind: "log", key: m.seq, source: m.source, lines: [m.content] });
      }
    } else {
      rows.push({ kind: "msg", key: m.seq, m });
    }
  }
  return rows;
}

function LogBlock({ lines, source }: { lines: string[]; source: string }) {
  return (
    <details className="group rounded-lg border border-edge/70 bg-raised/40">
      <summary className="cursor-pointer px-3 py-1.5 font-mono text-[11px] text-faint select-none">
        {source} · {lines.length} line{lines.length === 1 ? "" : "s"}
      </summary>
      <pre className="max-h-64 overflow-auto px-3 pb-2 font-mono text-[11px] whitespace-pre-wrap text-mute">
        {lines.join("\n")}
      </pre>
    </details>
  );
}

function MessageRow({ m }: { m: api.Message }) {
  if (m.kind === "usage") return null;
  const isUser = m.source === "user";
  if (m.kind === "status")
    return (
      <p className="text-center text-[11px] text-faint italic">{m.content}</p>
    );
  if (m.kind === "tool_call" || m.kind === "tool_result")
    return (
      <details className="group rounded-lg border border-edge/70 bg-raised/50">
        <summary className="cursor-pointer px-3 py-1.5 text-[11px] text-faint select-none">
          <span className="font-mono">
            {m.kind === "tool_call" ? "→ tool" : "← result"}
          </span>{" "}
          {firstLine(m.content)}
        </summary>
        <pre className="overflow-x-auto px-3 pb-2 font-mono text-[11px] whitespace-pre-wrap text-mute">
          {m.content}
        </pre>
      </details>
    );
  return (
    <div className={`max-w-[92%] ${isUser ? "self-end" : "self-start"}`}>
      <div
        className={`rounded-xl px-3 py-2 text-[13px] whitespace-pre-wrap ${
          m.kind === "error"
            ? "bg-rose-400/10 text-rose-300 ring-1 ring-rose-400/20"
            : isUser
              ? "bg-accent/15 text-ink ring-1 ring-accent/25"
              : "bg-raised text-ink/90 ring-1 ring-edge"
        }`}
      >
        {m.content}
      </div>
      <p
        className={`mt-0.5 flex gap-1.5 text-[10px] text-faint ${isUser ? "justify-end" : ""}`}
      >
        <span>{m.source}</span>
        <TimeAgo iso={m.created_at} />
      </p>
    </div>
  );
}

function firstLine(s: string): string {
  const line = s.split("\n", 1)[0];
  return line.length > 80 ? line.slice(0, 80) + "…" : line;
}

// TerminalPanel polls the tmux capture-pane endpoint while the session runs.
function TerminalPanel({ id, active }: { id: string; active: boolean }) {
  const [screen, setScreen] = useState<api.Screen | null>(null);
  const [gone, setGone] = useState(false);

  useEffect(() => {
    if (!active) {
      setGone(true);
      return;
    }
    let live = true;
    let timer: number | undefined;
    const tick = async () => {
      try {
        const s = await api.get<api.Screen | null>(`/api/sessions/${id}/screen`);
        if (!live) return;
        setScreen(s);
        setGone(s === null);
      } catch {
        // ignore; retry
      }
      if (live) timer = window.setTimeout(tick, 1000);
    };
    void tick();
    return () => {
      live = false;
      if (timer !== undefined) clearTimeout(timer);
    };
  }, [id, active]);

  if (gone && !screen)
    return (
      <Card className="px-4 py-8 text-center text-sm text-faint">
        No live terminal — the session isn't running in tmux right now.
      </Card>
    );

  return (
    <div className="overflow-hidden rounded-xl border border-edge bg-black">
      <div className="flex items-center justify-between border-b border-edge bg-surface px-3 py-1.5">
        <span className="flex items-center gap-1.5 text-[11px] text-faint">
          <span className="size-2 rounded-full bg-emerald-400/80" />
          tmux
        </span>
        {screen?.attach && (
          <span className="flex items-center gap-1 font-mono text-[11px] text-mute">
            {screen.attach}
            <CopyButton text={screen.attach} />
          </span>
        )}
      </div>
      <div className="term max-h-[26rem] min-h-[10rem] p-3 text-zinc-300">
        {screen?.screen || "(blank)"}
      </div>
    </div>
  );
}
