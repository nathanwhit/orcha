import { useState } from "react";
import * as api from "../api";
import { usePoll } from "../hooks";
import { Card, EmptyState } from "../ui";
import { SessionTable } from "./Objective";

const FILTERS = [
  { key: "all", label: "All" },
  { key: "active", label: "Active" },
  { key: "waiting", label: "Waiting" },
  { key: "done", label: "Done" },
] as const;

type FilterKey = (typeof FILTERS)[number]["key"];

function matches(filter: FilterKey, status: string): boolean {
  switch (filter) {
    case "all":
      return true;
    case "active":
      return ["running", "starting", "queued"].includes(status);
    case "waiting":
      return ["waiting_user", "waiting_capacity"].includes(status);
    case "done":
      return ["succeeded", "failed", "canceled"].includes(status);
  }
}

export function SessionsPage({ nav }: { nav: (to: string) => void }) {
  const sessions = usePoll(
    () => api.get<api.DashboardSession[]>("/api/sessions"),
    2000,
  );
  const [filter, setFilter] = useState<FilterKey>("all");

  const all = sessions.data ?? [];
  const shown = all.filter((s) => matches(filter, s.status));

  return (
    <div className="space-y-5">
      <div>
        <h1 className="text-xl font-semibold tracking-tight">Sessions</h1>
        <p className="mt-0.5 text-sm text-mute">
          Every agent session across all objectives.
        </p>
      </div>

      <div className="flex flex-wrap gap-1.5">
        {FILTERS.map((f) => {
          const n = all.filter((s) => matches(f.key, s.status)).length;
          return (
            <button
              key={f.key}
              type="button"
              onClick={() => setFilter(f.key)}
              className={`rounded-full px-3 py-1 text-xs font-medium ring-1 ring-inset transition-colors ${
                filter === f.key
                  ? "bg-accent/15 text-accent ring-accent/30"
                  : "text-mute ring-edge hover:bg-raised hover:text-ink"
              }`}
            >
              {f.label}
              <span className="ml-1.5 text-faint tabular-nums">{n}</span>
            </button>
          );
        })}
      </div>

      <Card className="overflow-x-auto">
        {shown.length === 0 ? (
          <EmptyState>No sessions{filter !== "all" && " in this state"}.</EmptyState>
        ) : (
          <SessionTable sessions={shown} nav={nav} />
        )}
      </Card>
      {sessions.error && (
        <p className="text-xs text-rose-400">{sessions.error}</p>
      )}
    </div>
  );
}
