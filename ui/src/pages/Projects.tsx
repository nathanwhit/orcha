import { useState } from "react";
import type { FormEvent } from "react";
import * as api from "../api";
import { usePoll } from "../hooks";
import { Icon } from "../icons";
import {
  Button,
  Card,
  Chip,
  EmptyState,
  Field,
  IconButton,
  Modal,
  TextInput,
  TimeAgo,
} from "../ui";

export function ProjectsPage() {
  const projects = usePoll(() => api.get<api.Project[]>("/api/projects"), 5000);
  const [adding, setAdding] = useState(false);
  const [editing, setEditing] = useState<api.Project | null>(null);
  const ps = projects.data ?? [];

  return (
    <div className="space-y-5">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h1 className="text-xl font-semibold tracking-tight">Projects</h1>
          <p className="mt-0.5 text-sm text-mute">
            Registered repositories — pick one when creating an objective
            instead of typing. Repos used once are remembered automatically.
          </p>
        </div>
        <Button variant="primary" onClick={() => setAdding(true)}>
          <Icon name="plus" className="size-3.5" />
          Add project
        </Button>
      </div>

      <Card>
        {ps.length === 0 ? (
          <EmptyState>
            No projects yet — add one, or just create an objective with a repo
            and it'll be remembered.
          </EmptyState>
        ) : (
          <div className="divide-y divide-edge/60">
            {ps.map((p) => (
              <div key={p.id} className="flex items-center gap-3 px-4 py-3">
                <Icon name="pr" className="size-4 shrink-0 text-mute" />
                <div className="min-w-0 flex-1">
                  <p className="truncate text-sm font-medium">
                    {p.name}
                    {p.name !== p.repo && (
                      <span className="ml-2 font-mono text-[11px] text-faint">
                        {p.repo}
                      </span>
                    )}
                  </p>
                  <p className="mt-0.5 flex flex-wrap items-center gap-1.5 text-[11px] text-faint">
                    {p.push_repo && (
                      <Chip>
                        pushes to fork{" "}
                        <span className="ml-1 font-mono">{p.push_repo}</span>
                      </Chip>
                    )}
                    <Chip>base {p.base_branch || "main"}</Chip>
                  </p>
                </div>
                <TimeAgo iso={p.updated_at} />
                <IconButton
                  name="edit"
                  title="Edit project"
                  onClick={() => setEditing(p)}
                />
                <IconButton
                  name="x"
                  title="Remove project"
                  onClick={() => {
                    if (confirm(`Remove ${p.name} from the registry?`))
                      void api
                        .del(`/api/projects/${p.id}`)
                        .then(projects.reload);
                  }}
                />
              </div>
            ))}
          </div>
        )}
      </Card>
      {projects.error && (
        <p className="text-xs text-rose-400">{projects.error}</p>
      )}

      {adding && (
        <ProjectModal
          onClose={() => setAdding(false)}
          onSaved={() => {
            setAdding(false);
            projects.reload();
          }}
        />
      )}
      {editing && (
        <ProjectModal
          project={editing}
          onClose={() => setEditing(null)}
          onSaved={() => {
            setEditing(null);
            projects.reload();
          }}
        />
      )}
    </div>
  );
}

// ProjectModal both adds and edits a project. With no `project` it POSTs a new
// one ("Add project"); with one it pre-fills the fields and PUTs the edit
// ("Edit project").
function ProjectModal({
  project,
  onClose,
  onSaved,
}: {
  project?: api.Project;
  onClose: () => void;
  onSaved: () => void;
}) {
  const editing = project != null;
  const [name, setName] = useState(project?.name ?? "");
  const [repo, setRepo] = useState(project?.repo ?? "");
  const [pushRepo, setPushRepo] = useState(project?.push_repo ?? "");
  const [base, setBase] = useState(project?.base_branch ?? "");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (busy) return;
    setBusy(true);
    setErr(null);
    try {
      const body = {
        name,
        repo,
        push_repo: pushRepo,
        base_branch: base,
      };
      if (editing) {
        await api.put(`/api/projects/${project.id}`, body);
      } else {
        await api.post("/api/projects", body);
      }
      onSaved();
    } catch (ex) {
      setErr(ex instanceof Error ? ex.message : String(ex));
      setBusy(false);
    }
  };

  return (
    <Modal title={editing ? "Edit project" : "Add project"} onClose={onClose}>
      <form onSubmit={submit} className="space-y-4">
        <div className="grid grid-cols-2 gap-3">
          <Field label="Name" hint="optional">
            <TextInput
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="defaults to repo"
              autoFocus
            />
          </Field>
          <Field label="Repo" hint="upstream — base & PR target">
            <TextInput
              value={repo}
              onChange={(e) => setRepo(e.target.value)}
              placeholder="owner/repo"
              required
            />
          </Field>
        </div>
        <div className="grid grid-cols-2 gap-3">
          <Field label="Push repo" hint="your fork; optional">
            <TextInput
              value={pushRepo}
              onChange={(e) => setPushRepo(e.target.value)}
              placeholder="you/repo"
            />
          </Field>
          <Field label="Base branch" hint="optional">
            <TextInput
              value={base}
              onChange={(e) => setBase(e.target.value)}
              placeholder="main"
            />
          </Field>
        </div>
        <p className="text-[11px] text-faint">
          With a push repo set, workers base their branches off the upstream,
          push them to the fork, and open PRs against the upstream — the
          standard fork workflow.
        </p>
        {err && <p className="text-xs text-rose-400">{err}</p>}
        <div className="flex justify-end gap-2 pt-1">
          <Button onClick={onClose}>Cancel</Button>
          <Button type="submit" variant="primary" disabled={busy || !repo}>
            {busy ? "Saving…" : "Save project"}
          </Button>
        </div>
      </form>
    </Modal>
  );
}
