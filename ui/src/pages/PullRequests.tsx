import { useState } from "react";
import * as api from "../api";
import { usePoll } from "../hooks";
import { Icon } from "../icons";
import { Card, EmptyState, IconButton, Pill, Td, Th, TimeAgo } from "../ui";

export function PullRequestsPage() {
  const prs = usePoll(
    () => api.get<api.PullRequest[]>("/api/pull-requests"),
    3000,
  );
  const [note, setNote] = useState<string | null>(null);

  const rows = prs.data ?? [];

  return (
    <div className="space-y-5">
      <div>
        <h1 className="text-xl font-semibold tracking-tight">Pull requests</h1>
        <p className="mt-0.5 text-sm text-mute">
          PRs the team has opened. Sync scans for new review comments and
          spawns follow-up agents.
        </p>
      </div>

      {note && <p className="text-xs text-accent">{note}</p>}

      <Card className="overflow-x-auto">
        {rows.length === 0 ? (
          <EmptyState>No pull requests yet.</EmptyState>
        ) : (
          <table className="w-full min-w-[720px] text-[13px]">
            <thead className="border-b border-edge">
              <tr>
                <Th>PR</Th>
                <Th>Repo</Th>
                <Th>Branch</Th>
                <Th>Status</Th>
                <Th>Checks</Th>
                <Th>Updated</Th>
                <Th />
              </tr>
            </thead>
            <tbody className="divide-y divide-edge/60">
              {rows.map((pr) => (
                <tr key={pr.id} className="transition-colors hover:bg-raised/40">
                  <Td className="max-w-[320px]">
                    <a
                      href={pr.url}
                      target="_blank"
                      rel="noreferrer"
                      className="flex items-center gap-1.5 font-medium hover:text-accent"
                    >
                      <span className="truncate">
                        #{pr.number} {pr.title}
                      </span>
                      <Icon name="external" className="size-3 shrink-0 text-faint" />
                    </a>
                  </Td>
                  <Td className="text-mute">{pr.repo}</Td>
                  <Td className="max-w-[200px] truncate font-mono text-[11px] text-faint">
                    {pr.branch} → {pr.base_branch}
                  </Td>
                  <Td>
                    <Pill status={pr.status} />
                  </Td>
                  <Td>
                    <Pill status={pr.checks_state} />
                  </Td>
                  <Td>
                    <TimeAgo iso={pr.updated_at} />
                  </Td>
                  <Td className="whitespace-nowrap">
                    <IconButton
                      name="refresh"
                      title="Refresh state from GitHub"
                      onClick={() =>
                        void api
                          .post(`/api/pull-requests/${pr.id}/refresh`)
                          .then(prs.reload)
                      }
                    />
                    <IconButton
                      name="send"
                      title="Sync comments & spawn follow-ups"
                      onClick={() =>
                        void api
                          .post<{ followups_spawned: string[] }>(
                            `/api/pull-requests/${pr.id}/sync`,
                          )
                          .then((r) => {
                            setNote(
                              r.followups_spawned.length > 0
                                ? `Spawned ${r.followups_spawned.length} follow-up session(s) for PR #${pr.number}.`
                                : `No new actionable feedback on PR #${pr.number}.`,
                            );
                            prs.reload();
                          })
                          .catch((e: Error) => setNote(e.message))
                      }
                    />
                  </Td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Card>
      {prs.error && <p className="text-xs text-rose-400">{prs.error}</p>}
    </div>
  );
}
