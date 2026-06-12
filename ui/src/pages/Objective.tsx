import { useState } from "react";
import * as api from "../api";
import { usePoll } from "../hooks";
import { Icon } from "../icons";
import {
  Button,
  Card,
  Chip,
  EmptyState,
  Pill,
  SectionTitle,
  SteerBox,
  Td,
  Th,
  TimeAgo,
} from "../ui";
import { QuestionInbox } from "./Overview";

export function ObjectivePage({
  id,
  nav,
}: {
  id: string;
  nav: (to: string) => void;
}) {
  const detail = usePoll(
    () => api.get<api.ObjectiveDetail>(`/api/objectives/${id}`),
    2000,
    [id],
  );
  const [showPrompt, setShowPrompt] = useState(false);

  if (detail.error && !detail.data)
    return <p className="text-sm text-rose-400">{detail.error}</p>;
  if (!detail.data) return <p className="text-sm text-faint">Loading…</p>;

  // Defend against null lists from older servers (Go nil slices encode as null).
  const obj = detail.data.objective;
  const sessions = detail.data.sessions ?? [];
  const pull_requests = detail.data.pull_requests ?? [];
  const questions = detail.data.questions ?? [];
  const artifacts = detail.data.artifacts ?? [];
  const usage = detail.data.usage;
  const openQs = questions.filter((q) => q.status === "open");
  const terminal = ["succeeded", "failed", "canceled"].includes(obj.status);

  return (
    <div className="space-y-6">
      <div>
        <a
          href="#/"
          className="mb-3 inline-flex items-center gap-1 text-xs text-mute transition-colors hover:text-ink"
        >
          <Icon name="back" className="size-3.5" />
          Overview
        </a>
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div className="min-w-0">
            <div className="flex flex-wrap items-center gap-3">
              <h1 className="text-xl font-semibold tracking-tight">
                {obj.title}
              </h1>
              <Pill status={obj.status} />
            </div>
            <p className="mt-1 flex flex-wrap items-center gap-2 text-xs text-faint">
              <span className="font-mono">{api.shortId(obj.id)}</span>
              {typeof obj.metadata?.repo === "string" && (
                <Chip>{obj.metadata.repo}</Chip>
              )}
              <span>
                created <TimeAgo iso={obj.created_at} />
              </span>
            </p>
          </div>
          {!terminal && (
            <Button
              variant="danger"
              onClick={() => {
                if (confirm("Cancel this objective and all its sessions?"))
                  void api
                    .post(`/api/objectives/${id}/cancel`)
                    .then(detail.reload);
              }}
            >
              <Icon name="stop" className="size-3.5" />
              Cancel
            </Button>
          )}
        </div>
      </div>

      {obj.summary && (
        <Card className="border-sky-400/20 px-4 py-3">
          <p className="text-[11px] font-semibold tracking-wider text-sky-300 uppercase">
            Outcome
          </p>
          <p className="mt-1 text-sm whitespace-pre-wrap">{obj.summary}</p>
        </Card>
      )}

      <Card className="px-4 py-3">
        <button
          type="button"
          onClick={() => setShowPrompt((v) => !v)}
          className="flex w-full items-center justify-between text-left text-[11px] font-semibold tracking-wider text-faint uppercase"
        >
          Prompt
          <span className="text-faint">{showPrompt ? "hide" : "show"}</span>
        </button>
        {showPrompt && (
          <p className="mt-2 text-sm whitespace-pre-wrap text-mute">
            {obj.prompt}
          </p>
        )}
      </Card>

      {openQs.length > 0 && (
        <QuestionInbox questions={openQs} onAnswered={detail.reload} />
      )}

      {!terminal && obj.manager_session_id && (
        <SteerBox
          placeholder="Tell the manager something — redirect, reprioritize, add context…"
          onSend={(message) =>
            api.post(`/api/objectives/${id}/steer`, { message })
          }
        />
      )}

      <section>
        <SectionTitle
          right={
            usage && usage.total_tokens > 0 ? (
              <span className="flex flex-wrap items-center gap-1.5">
                <Chip>{api.formatTokens(usage.total_tokens)} tokens</Chip>
                {usage.providers.map((p) => (
                  <Chip key={p.provider}>
                    {p.provider} {api.formatTokens(p.used_tokens)}
                  </Chip>
                ))}
              </span>
            ) : undefined
          }
        >
          Sessions
        </SectionTitle>
        <Card className="overflow-x-auto">
          {sessions.length === 0 ? (
            <EmptyState>No sessions yet.</EmptyState>
          ) : (
            <SessionTable sessions={sessions} nav={nav} />
          )}
        </Card>
      </section>

      <div className="grid gap-6 lg:grid-cols-2">
        <section>
          <SectionTitle>Pull requests</SectionTitle>
          <Card>
            {pull_requests.length === 0 ? (
              <EmptyState>None yet.</EmptyState>
            ) : (
              <div className="divide-y divide-edge/60">
                {pull_requests.map((pr) => (
                  <a
                    key={pr.id}
                    href={pr.url}
                    target="_blank"
                    rel="noreferrer"
                    className="flex items-center gap-3 px-4 py-3 transition-colors hover:bg-raised/60"
                  >
                    <Icon name="pr" className="size-4 shrink-0 text-mute" />
                    <div className="min-w-0 flex-1">
                      <p className="truncate text-sm font-medium">
                        #{pr.number} {pr.title}
                      </p>
                      <p className="truncate font-mono text-[11px] text-faint">
                        {pr.branch} → {pr.base_branch}
                      </p>
                    </div>
                    <Pill status={pr.status} />
                    <Pill status={pr.checks_state} />
                  </a>
                ))}
              </div>
            )}
          </Card>
        </section>

        <section>
          <SectionTitle>Artifacts & notes</SectionTitle>
          <Card>
            {artifacts.length === 0 ? (
              <EmptyState>Nothing recorded yet.</EmptyState>
            ) : (
              <div className="divide-y divide-edge/60">
                {artifacts.map((a) => (
                  <div key={a.id} className="px-4 py-3">
                    <div className="flex items-center gap-2">
                      <Chip>{a.kind.replaceAll("_", " ")}</Chip>
                      <p className="min-w-0 flex-1 truncate text-sm font-medium">
                        {a.uri ? (
                          <a
                            href={a.uri}
                            target="_blank"
                            rel="noreferrer"
                            className="hover:text-accent"
                          >
                            {a.title}
                          </a>
                        ) : (
                          a.title
                        )}
                      </p>
                      <TimeAgo iso={a.created_at} />
                    </div>
                    {a.summary && (
                      <p className="mt-1 line-clamp-3 text-xs whitespace-pre-wrap text-mute">
                        {a.summary}
                      </p>
                    )}
                  </div>
                ))}
              </div>
            )}
          </Card>
        </section>
      </div>
    </div>
  );
}

export function SessionTable({
  sessions,
  nav,
}: {
  sessions: api.DashboardSession[];
  nav: (to: string) => void;
}) {
  return (
    <table className="w-full min-w-[640px] text-[13px]">
      <thead className="border-b border-edge">
        <tr>
          <Th>Status</Th>
          <Th>Title</Th>
          <Th>Role</Th>
          <Th>Agent</Th>
          <Th>Tokens</Th>
          <Th>Activity</Th>
          <Th>Updated</Th>
        </tr>
      </thead>
      <tbody className="divide-y divide-edge/60">
        {sessions.map((s) => (
          <tr
            key={s.id}
            onClick={() => nav(`/sessions/${s.id}`)}
            className="cursor-pointer transition-colors hover:bg-raised/60"
          >
            <Td>
              <Pill status={s.status} />
            </Td>
            <Td className="max-w-[280px] truncate font-medium">
              {s.title || "(untitled)"}
            </Td>
            <Td>
              <Chip>{s.role.replaceAll("_", " ")}</Chip>
            </Td>
            <Td className="text-mute">{s.agent}</Td>
            <Td className="font-mono text-mute tabular-nums">
              {s.used_tokens > 0 ? api.formatTokens(s.used_tokens) : "—"}
            </Td>
            <Td className="max-w-[260px] truncate text-mute">
              {s.current_activity || "—"}
            </Td>
            <Td>
              <TimeAgo iso={s.updated_at} />
            </Td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}
