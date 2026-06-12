import { useState } from "react";
import type { FormEvent, KeyboardEvent } from "react";
import * as api from "../api";
import { usePoll } from "../hooks";
import { Icon } from "../icons";
import {
  Button,
  Card,
  Collapsible,
  EmptyState,
  Field,
  Modal,
  Pill,
  SectionTitle,
  Select,
  Td,
  TextArea,
  TextInput,
  Th,
  TimeAgo,
} from "../ui";

export function Overview({ nav }: { nav: (to: string) => void }) {
  const objectives = usePoll(
    () => api.get<api.DashboardObjective[]>("/api/objectives"),
    2000,
  );
  const sessions = usePoll(
    () => api.get<api.DashboardSession[]>("/api/sessions"),
    2000,
  );
  const prs = usePoll(
    () => api.get<api.PullRequest[]>("/api/pull-requests"),
    4000,
  );
  const questions = usePoll(
    () => api.get<api.Question[]>("/api/questions"),
    3000,
  );
  const [creating, setCreating] = useState(false);

  const objs = objectives.data ?? [];
  const activeObjs = objs.filter((o) => o.status !== "canceled");
  const canceledObjs = objs.filter((o) => o.status === "canceled");
  const running = (sessions.data ?? []).filter(
    (s) => s.status === "running",
  ).length;
  const openPRs = (prs.data ?? []).filter((p) => p.status === "open").length;
  const qs = questions.data ?? [];

  return (
    <div className="space-y-6">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h1 className="text-xl font-semibold tracking-tight">Overview</h1>
          <p className="mt-0.5 text-sm text-mute">
            Objectives, the teams working them, and what needs you.
          </p>
        </div>
        <Button variant="primary" onClick={() => setCreating(true)}>
          <Icon name="plus" className="size-3.5" />
          New objective
        </Button>
      </div>

      <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
        <Stat
          label="Active objectives"
          value={objs.filter((o) => o.status === "active").length}
        />
        <Stat label="Running sessions" value={running} />
        <Stat label="Open PRs" value={openPRs} />
        <Stat label="Needs you" value={qs.length} highlight={qs.length > 0} />
      </div>

      {qs.length > 0 && <QuestionInbox questions={qs} onAnswered={questions.reload} />}

      <section>
        <SectionTitle>Objectives</SectionTitle>
        <Card className="overflow-x-auto">
          {objs.length === 0 ? (
            <EmptyState>
              No objectives yet — create one and a manager session will start
              planning.
            </EmptyState>
          ) : activeObjs.length === 0 ? (
            <EmptyState>No active objectives.</EmptyState>
          ) : (
            <ObjectivesTable objectives={activeObjs} nav={nav} />
          )}
        </Card>
        {objectives.error && (
          <p className="mt-2 text-xs text-rose-400">{objectives.error}</p>
        )}
      </section>

      {canceledObjs.length > 0 && (
        <section>
          <Collapsible title="Canceled objectives" count={canceledObjs.length}>
            <Card className="overflow-x-auto">
              <ObjectivesTable objectives={canceledObjs} nav={nav} />
            </Card>
          </Collapsible>
        </section>
      )}

      {creating && (
        <NewObjectiveModal
          onClose={() => setCreating(false)}
          onCreated={(id) => {
            setCreating(false);
            nav(`/objectives/${id}`);
          }}
        />
      )}
    </div>
  );
}

function ObjectivesTable({
  objectives,
  nav,
}: {
  objectives: api.DashboardObjective[];
  nav: (to: string) => void;
}) {
  return (
    <table className="w-full min-w-[640px] text-[13px]">
      <thead className="border-b border-edge">
        <tr>
          <Th>Status</Th>
          <Th>Title</Th>
          <Th>Repo</Th>
          <Th>Sessions</Th>
          <Th>PRs</Th>
          <Th>Activity</Th>
          <Th>Updated</Th>
        </tr>
      </thead>
      <tbody className="divide-y divide-edge/60">
        {objectives.map((o) => (
          <tr
            key={o.id}
            onClick={() => nav(`/objectives/${o.id}`)}
            className="cursor-pointer transition-colors hover:bg-raised/60"
          >
            <Td>
              <span className="flex items-center gap-2">
                <Pill status={o.status} />
                {o.needs_user && <Pill status="waiting_user" />}
              </span>
            </Td>
            <Td className="font-medium">{o.title}</Td>
            <Td className="text-mute">{o.repo || "—"}</Td>
            <Td className="text-mute">{o.active_sessions}</Td>
            <Td className="text-mute">{o.pr_count}</Td>
            <Td className="max-w-[260px] truncate text-mute">
              {o.latest_activity || "—"}
            </Td>
            <Td>
              <TimeAgo iso={o.updated_at} />
            </Td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

function Stat({
  label,
  value,
  highlight,
}: {
  label: string;
  value: number;
  highlight?: boolean;
}) {
  return (
    <Card className="px-4 py-3">
      <p className="text-[11px] font-medium tracking-wider text-faint uppercase">
        {label}
      </p>
      <p
        className={`mt-1 text-2xl font-semibold tabular-nums ${highlight ? "text-amber-300" : ""}`}
      >
        {value}
      </p>
    </Card>
  );
}

export function QuestionInbox({
  questions,
  onAnswered,
}: {
  questions: api.Question[];
  onAnswered: () => void;
}) {
  return (
    <Card className="border-amber-400/20">
      <div className="flex items-center gap-2 border-b border-edge px-4 py-3">
        <Icon name="help" className="size-4 text-amber-300" />
        <h2 className="text-sm font-semibold">Waiting on you</h2>
      </div>
      <div className="divide-y divide-edge/60">
        {questions.map((q) => (
          <QuestionRow key={q.id} q={q} onAnswered={onAnswered} />
        ))}
      </div>
    </Card>
  );
}

function QuestionRow({
  q,
  onAnswered,
}: {
  q: api.Question;
  onAnswered: () => void;
}) {
  const [answer, setAnswer] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (!answer.trim() || busy) return;
    setBusy(true);
    setErr(null);
    try {
      await api.post(`/api/questions/${q.id}/answer`, { answer: answer.trim() });
      onAnswered();
    } catch (ex) {
      setErr(ex instanceof Error ? ex.message : String(ex));
      setBusy(false);
    }
  };
  return (
    <div className="px-4 py-3">
      <p className="text-sm">{q.question}</p>
      {q.context && <p className="mt-1 text-xs text-mute">{q.context}</p>}
      <form onSubmit={submit} className="mt-2 flex items-center gap-2">
        <TextInput
          value={answer}
          onChange={(e) => setAnswer(e.target.value)}
          placeholder="Your answer…"
        />
        <Button type="submit" variant="primary" disabled={busy || !answer.trim()}>
          Answer
        </Button>
      </form>
      {err && <p className="mt-1 text-xs text-rose-400">{err}</p>}
    </div>
  );
}

function NewObjectiveModal({
  onClose,
  onCreated,
}: {
  onClose: () => void;
  onCreated: (id: string) => void;
}) {
  const [title, setTitle] = useState("");
  const [prompt, setPrompt] = useState("");
  const [projectID, setProjectID] = useState("");
  const [repo, setRepo] = useState("");
  const [pushRepo, setPushRepo] = useState("");
  const [base, setBase] = useState("");
  const [agent, setAgent] = useState("claude");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const projects = usePoll(
    () => api.get<api.Project[]>("/api/projects"),
    60000,
  );
  const ps = projects.data ?? [];
  const custom = projectID === "";

  const submit = async (e: FormEvent | KeyboardEvent) => {
    e.preventDefault();
    if (busy) return;
    setBusy(true);
    setErr(null);
    try {
      const res = await api.post<{ objective: api.Objective }>(
        "/api/objectives",
        custom
          ? { title, prompt, agent, repo, push_repo: pushRepo, base_branch: base }
          : { title, prompt, agent, project_id: projectID },
      );
      onCreated(res.objective.id);
    } catch (ex) {
      setErr(ex instanceof Error ? ex.message : String(ex));
      setBusy(false);
    }
  };

  return (
    <Modal title="New objective" onClose={onClose}>
      <form onSubmit={submit} className="space-y-4">
        <Field label="Title" hint="optional">
          <TextInput
            value={title}
            onChange={(e) => setTitle(e.target.value)}
            placeholder="Leave blank to auto-generate from the prompt"
            autoFocus
          />
        </Field>
        <Field label="What should the team do?">
          <TextArea
            value={prompt}
            onChange={(e) => setPrompt(e.target.value)}
            onKeyDown={(e) => {
              if ((e.metaKey || e.ctrlKey) && e.key === "Enter") submit(e);
            }}
            placeholder="Describe the goal, constraints, and what done looks like…"
            rows={5}
            required
          />
        </Field>
        <Field label="Project" hint="required for coding work">
          <Select
            value={projectID}
            onChange={(e) => setProjectID(e.target.value)}
          >
            <option value="">custom repo…</option>
            {ps.map((p) => (
              <option key={p.id} value={p.id}>
                {p.name === p.repo ? p.repo : `${p.name} (${p.repo})`}
                {p.push_repo ? " — via fork" : ""}
              </option>
            ))}
          </Select>
        </Field>
        {custom && (
          <>
            <div className="grid grid-cols-2 gap-3">
              <Field label="Repo" hint="upstream owner/repo">
                <TextInput
                  value={repo}
                  onChange={(e) => setRepo(e.target.value)}
                  placeholder="owner/repo"
                />
              </Field>
              <Field label="Push repo" hint="fork; optional">
                <TextInput
                  value={pushRepo}
                  onChange={(e) => setPushRepo(e.target.value)}
                  placeholder="you/repo"
                />
              </Field>
            </div>
            <Field label="Base branch" hint="optional">
              <TextInput
                value={base}
                onChange={(e) => setBase(e.target.value)}
                placeholder="main"
              />
            </Field>
            <p className="text-[11px] text-faint">
              A typed repo is remembered as a project for next time.
            </p>
          </>
        )}
        <Field label="Manager agent">
          <Select value={agent} onChange={(e) => setAgent(e.target.value)}>
            <option value="claude">claude</option>
            <option value="codex">codex</option>
          </Select>
        </Field>
        {err && <p className="text-xs text-rose-400">{err}</p>}
        <div className="flex justify-end gap-2 pt-1">
          <Button onClick={onClose}>Cancel</Button>
          <Button type="submit" variant="primary" disabled={busy}>
            {busy ? "Creating…" : "Create & start"}
          </Button>
        </div>
      </form>
    </Modal>
  );
}
