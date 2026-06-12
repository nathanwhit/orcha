import * as api from "../api";
import { usePoll } from "../hooks";
import { Card, Chip, EmptyState, TimeAgo } from "../ui";

export function ActivityPage() {
  const events = usePoll(
    () => api.get<api.OrchaEvent[]>("/api/events?limit=300"),
    2000,
  );

  const rows = [...(events.data ?? [])].reverse(); // newest first

  return (
    <div className="space-y-5">
      <div>
        <h1 className="text-xl font-semibold tracking-tight">Activity</h1>
        <p className="mt-0.5 text-sm text-mute">
          The audit trail: every orchestrator decision and state change.
        </p>
      </div>

      <Card>
        {rows.length === 0 ? (
          <EmptyState>No events yet.</EmptyState>
        ) : (
          <div className="divide-y divide-edge/60">
            {rows.map((e) => (
              <div key={e.id} className="flex items-baseline gap-3 px-4 py-2.5">
                <Chip>{e.type.replaceAll("_", " ")}</Chip>
                <p className="min-w-0 flex-1 truncate text-[13px] text-ink/90">
                  {e.summary}
                  {e.session_id && (
                    <a
                      href={`#/sessions/${e.session_id}`}
                      className="ml-2 font-mono text-[11px] text-faint hover:text-accent"
                    >
                      {api.shortId(e.session_id)}
                    </a>
                  )}
                </p>
                <TimeAgo iso={e.created_at} />
              </div>
            ))}
          </div>
        )}
      </Card>
      {events.error && <p className="text-xs text-rose-400">{events.error}</p>}
    </div>
  );
}
