import { useState } from "react";
import type { FormEvent } from "react";
import * as api from "../api";
import { usePoll } from "../hooks";
import { Icon } from "../icons";
import {
  Button,
  Card,
  Chip,
  Field,
  Modal,
  Pill,
  TextInput,
  TimeAgo,
} from "../ui";

export function TargetsPage() {
  const targets = usePoll(() => api.get<api.Target[]>("/api/targets"), 3000);
  const [adding, setAdding] = useState(false);

  return (
    <div className="space-y-5">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h1 className="text-xl font-semibold tracking-tight">Targets</h1>
          <p className="mt-0.5 text-sm text-mute">
            Machines that run agent sessions — local and over SSH.
          </p>
        </div>
        <Button variant="primary" onClick={() => setAdding(true)}>
          <Icon name="plus" className="size-3.5" />
          Add SSH machine
        </Button>
      </div>

      <div className="grid gap-4 md:grid-cols-2">
        {(targets.data ?? []).map((t) => (
          <TargetCard key={t.id} t={t} onChanged={targets.reload} />
        ))}
      </div>
      {targets.error && <p className="text-xs text-rose-400">{targets.error}</p>}

      {adding && (
        <AddTargetModal
          onClose={() => setAdding(false)}
          onAdded={targets.reload}
        />
      )}
    </div>
  );
}

function TargetCard({ t, onChanged }: { t: api.Target; onChanged: () => void }) {
  const [doctor, setDoctor] = useState<api.DoctorReport | null>(null);
  const [busy, setBusy] = useState<string | null>(null);

  const act = async (name: string, fn: () => Promise<unknown>) => {
    setBusy(name);
    try {
      await fn();
      onChanged();
    } finally {
      setBusy(null);
    }
  };

  const used = t.capacity_sessions - t.available_sessions;
  const pct =
    t.capacity_sessions > 0
      ? Math.round((used / t.capacity_sessions) * 100)
      : 0;

  return (
    <Card className="flex flex-col">
      <div className="flex items-start justify-between gap-3 px-4 pt-4">
        <div className="min-w-0">
          <div className="flex items-center gap-2">
            <Icon name="server" className="size-4 text-mute" />
            <h3 className="truncate text-sm font-semibold">{t.name}</h3>
            <Chip>{t.kind}</Chip>
          </div>
          <p className="mt-1 truncate font-mono text-[11px] text-faint">
            {t.kind === "ssh"
              ? `${t.user ? t.user + "@" : ""}${t.host}:${t.work_root}`
              : t.work_root}
          </p>
        </div>
        <Pill status={t.status} />
      </div>

      <div className="px-4 pt-3">
        <div className="flex items-center justify-between text-[11px] text-faint">
          <span>
            {used}/{t.capacity_sessions} sessions
          </span>
          {t.last_seen_at && (
            <span>
              seen <TimeAgo iso={t.last_seen_at} />
            </span>
          )}
        </div>
        <div className="mt-1.5 h-1.5 overflow-hidden rounded-full bg-raised">
          <div
            className={`h-full rounded-full transition-all ${pct >= 100 ? "bg-amber-400/80" : "bg-accent/80"}`}
            style={{ width: `${Math.min(pct, 100)}%` }}
          />
        </div>
        {t.labels && t.labels.length > 0 && (
          <div className="mt-2.5 flex flex-wrap gap-1">
            {t.labels.map((l) => (
              <Chip key={l}>{l}</Chip>
            ))}
          </div>
        )}
      </div>

      {doctor && (
        <div className="mx-4 mt-3 rounded-lg border border-edge bg-raised/50 p-3">
          <div className="flex items-center justify-between">
            <p className="text-[11px] font-semibold tracking-wider text-faint uppercase">
              Doctor {doctor.ok ? "— healthy" : "— problems"}
            </p>
            <button
              type="button"
              onClick={() => setDoctor(null)}
              className="text-faint hover:text-ink"
            >
              <Icon name="x" className="size-3.5" />
            </button>
          </div>
          <ul className="mt-2 space-y-1">
            {doctor.checks.map((c) => (
              <li key={c.name} className="flex items-center gap-2 text-xs">
                <Icon
                  name={c.ok ? "check" : "alert"}
                  className={`size-3.5 shrink-0 ${c.ok ? "text-emerald-400" : c.required ? "text-rose-400" : "text-amber-400"}`}
                />
                <span className="font-mono">{c.name}</span>
                {c.detail && (
                  <span className="truncate text-faint">{c.detail}</span>
                )}
              </li>
            ))}
          </ul>
        </div>
      )}

      <div className="mt-auto flex flex-wrap gap-2 px-4 py-3.5">
        <Button
          disabled={busy !== null}
          onClick={() =>
            act("doctor", async () => {
              const rep = await api.post<api.DoctorReport>(
                `/api/targets/${t.id}/doctor`,
              );
              setDoctor(rep);
            })
          }
        >
          <Icon name="stethoscope" className="size-3.5" />
          {busy === "doctor" ? "Checking…" : "Doctor"}
        </Button>
        {t.status === "online" && (
          <Button
            disabled={busy !== null}
            onClick={() =>
              act("drain", () => api.post(`/api/targets/${t.id}/drain`))
            }
          >
            Drain
          </Button>
        )}
        {t.status !== "online" && (
          <Button
            disabled={busy !== null}
            onClick={() =>
              act("enable", () => api.post(`/api/targets/${t.id}/enable`))
            }
          >
            Enable
          </Button>
        )}
        {t.status !== "disabled" && (
          <Button
            variant="danger"
            disabled={busy !== null}
            onClick={() =>
              act("disable", () => api.post(`/api/targets/${t.id}/disable`))
            }
          >
            Disable
          </Button>
        )}
      </div>
    </Card>
  );
}

function AddTargetModal({
  onClose,
  onAdded,
}: {
  onClose: () => void;
  onAdded: () => void;
}) {
  const [name, setName] = useState("");
  const [host, setHost] = useState("");
  const [user, setUser] = useState("");
  const [workRoot, setWorkRoot] = useState("/home/bot/work");
  const [capacity, setCapacity] = useState("2");
  const [labels, setLabels] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [result, setResult] = useState<{
    target: api.Target;
    doctor: api.DoctorReport | null;
  } | null>(null);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (busy) return;
    setBusy(true);
    setErr(null);
    try {
      const res = await api.post<{
        target: api.Target;
        doctor: api.DoctorReport | null;
      }>("/api/targets", {
        name,
        kind: "ssh",
        host,
        user,
        work_root: workRoot,
        capacity_sessions: parseInt(capacity, 10) || 2,
        labels: labels
          .split(",")
          .map((s) => s.trim())
          .filter(Boolean),
      });
      setResult(res);
      onAdded();
    } catch (ex) {
      setErr(ex instanceof Error ? ex.message : String(ex));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Modal title="Add SSH machine" onClose={onClose}>
      {result ? (
        <div className="space-y-4">
          <div className="flex items-center gap-2">
            <Pill status={result.target.status} />
            <p className="text-sm">
              {result.target.name} registered
              {result.doctor && !result.doctor.ok && (
                <span className="text-rose-300">
                  {" "}
                  — missing: {result.doctor.missing.join(", ")}
                </span>
              )}
            </p>
          </div>
          {result.doctor && (
            <ul className="space-y-1">
              {result.doctor.checks.map((c) => (
                <li key={c.name} className="flex items-center gap-2 text-xs">
                  <Icon
                    name={c.ok ? "check" : "alert"}
                    className={`size-3.5 ${c.ok ? "text-emerald-400" : c.required ? "text-rose-400" : "text-amber-400"}`}
                  />
                  <span className="font-mono">{c.name}</span>
                  {c.detail && (
                    <span className="truncate text-faint">{c.detail}</span>
                  )}
                </li>
              ))}
            </ul>
          )}
          <div className="flex justify-end">
            <Button variant="primary" onClick={onClose}>
              Done
            </Button>
          </div>
        </div>
      ) : (
        <form onSubmit={submit} className="space-y-4">
          <div className="grid grid-cols-2 gap-3">
            <Field label="Name">
              <TextInput
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="buildbox"
                required
                autoFocus
              />
            </Field>
            <Field label="Host">
              <TextInput
                value={host}
                onChange={(e) => setHost(e.target.value)}
                placeholder="10.0.0.5 or box.example.com"
                required
              />
            </Field>
          </div>
          <div className="grid grid-cols-2 gap-3">
            <Field label="User">
              <TextInput
                value={user}
                onChange={(e) => setUser(e.target.value)}
                placeholder="bot"
              />
            </Field>
            <Field label="Capacity" hint="parallel sessions">
              <TextInput
                value={capacity}
                onChange={(e) => setCapacity(e.target.value)}
                inputMode="numeric"
              />
            </Field>
          </div>
          <Field label="Work root">
            <TextInput
              value={workRoot}
              onChange={(e) => setWorkRoot(e.target.value)}
              required
            />
          </Field>
          <Field label="Labels" hint="comma-separated, optional">
            <TextInput
              value={labels}
              onChange={(e) => setLabels(e.target.value)}
              placeholder="gpu, linux"
            />
          </Field>
          {err && <p className="text-xs text-rose-400">{err}</p>}
          <p className="text-[11px] text-faint">
            The machine is health-checked and a doctor run verifies tmux, git,
            gh, and agent CLIs before it comes online. This can take a few
            seconds.
          </p>
          <div className="flex justify-end gap-2">
            <Button onClick={onClose}>Cancel</Button>
            <Button type="submit" variant="primary" disabled={busy}>
              {busy ? "Checking…" : "Register & check"}
            </Button>
          </div>
        </form>
      )}
    </Modal>
  );
}
