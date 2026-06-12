import { useState } from "react";
import * as api from "./api";
import { useHashPath, usePoll } from "./hooks";
import { Icon } from "./icons";
import type { IconName } from "./icons";
import { ActivityPage } from "./pages/Activity";
import { ObjectivePage } from "./pages/Objective";
import { Overview } from "./pages/Overview";
import { PullRequestsPage } from "./pages/PullRequests";
import { SessionPage } from "./pages/SessionDetail";
import { SessionsPage } from "./pages/Sessions";
import { TargetsPage } from "./pages/Targets";

const NAV: { to: string; label: string; icon: IconName }[] = [
  { to: "/", label: "Overview", icon: "grid" },
  { to: "/sessions", label: "Sessions", icon: "terminal" },
  { to: "/prs", label: "Pull requests", icon: "pr" },
  { to: "/targets", label: "Targets", icon: "server" },
  { to: "/activity", label: "Activity", icon: "activity" },
];

export default function App() {
  const [path, nav] = useHashPath();
  const [menuOpen, setMenuOpen] = useState(false);
  const seg = path.split("/").filter(Boolean);

  let page: React.ReactNode;
  if (seg.length === 0) page = <Overview nav={nav} />;
  else if (seg[0] === "objectives" && seg[1])
    page = <ObjectivePage id={seg[1]} nav={nav} />;
  else if (seg[0] === "sessions" && seg[1]) page = <SessionPage id={seg[1]} />;
  else if (seg[0] === "sessions") page = <SessionsPage nav={nav} />;
  else if (seg[0] === "prs") page = <PullRequestsPage />;
  else if (seg[0] === "targets") page = <TargetsPage />;
  else if (seg[0] === "activity") page = <ActivityPage />;
  else
    page = (
      <p className="text-sm text-faint">
        Nothing here. <a href="#/" className="text-accent">Back to overview.</a>
      </p>
    );

  const active = "/" + (seg[0] ?? "");

  return (
    <div className="min-h-dvh md:grid md:grid-cols-[220px_1fr]">
      {/* Mobile top bar */}
      <div className="sticky top-0 z-40 flex items-center justify-between border-b border-edge bg-bg/90 px-4 py-3 backdrop-blur md:hidden">
        <Logo />
        <button
          type="button"
          onClick={() => setMenuOpen((v) => !v)}
          className="rounded-md p-1.5 text-mute hover:text-ink"
        >
          <Icon name={menuOpen ? "x" : "menu"} className="size-5" />
        </button>
      </div>
      {menuOpen && (
        <div
          className="fixed inset-0 z-30 bg-black/50 md:hidden"
          onClick={() => setMenuOpen(false)}
        >
          <nav
            className="absolute top-12 right-3 left-3 rounded-xl border border-edge bg-surface p-2 shadow-2xl"
            onClick={(e) => e.stopPropagation()}
          >
            <NavItems
              active={active}
              onPick={(to) => {
                nav(to);
                setMenuOpen(false);
              }}
            />
          </nav>
        </div>
      )}

      {/* Desktop sidebar */}
      <aside className="sticky top-0 hidden h-dvh flex-col border-r border-edge bg-surface/50 md:flex">
        <div className="px-5 pt-5 pb-4">
          <Logo />
        </div>
        <nav className="flex-1 space-y-0.5 px-3">
          <NavItems active={active} onPick={nav} />
        </nav>
        <HealthFooter />
      </aside>

      <main className="mx-auto w-full max-w-6xl px-4 py-6 md:px-8 md:py-8">
        {page}
      </main>
    </div>
  );
}

function Logo() {
  return (
    <a href="#/" className="flex items-center gap-2.5">
      <span className="relative flex size-5 items-center justify-center">
        <span className="absolute inset-0 rounded-full border-2 border-accent" />
        <span className="size-1.5 rounded-full bg-accent" />
      </span>
      <span className="text-[15px] font-semibold tracking-tight">orcha</span>
    </a>
  );
}

function NavItems({
  active,
  onPick,
}: {
  active: string;
  onPick: (to: string) => void;
}) {
  const questions = usePoll(
    () => api.get<api.Question[]>("/api/questions"),
    5000,
  );
  const pending = (questions.data ?? []).length;
  return (
    <>
      {NAV.map((item) => {
        const isActive =
          item.to === "/" ? active === "/" : active.startsWith(item.to);
        return (
          <button
            key={item.to}
            type="button"
            onClick={() => onPick(item.to)}
            className={`flex w-full items-center gap-2.5 rounded-lg px-2.5 py-2 text-[13px] transition-colors ${
              isActive
                ? "bg-accent/12 font-medium text-accent"
                : "text-mute hover:bg-raised hover:text-ink"
            }`}
          >
            <Icon name={item.icon} className="size-4" />
            {item.label}
            {item.to === "/" && pending > 0 && (
              <span className="ml-auto rounded-full bg-amber-400/15 px-1.5 py-0.5 text-[10px] font-semibold text-amber-300 tabular-nums">
                {pending}
              </span>
            )}
          </button>
        );
      })}
    </>
  );
}

function HealthFooter() {
  const health = usePoll(() => api.get<api.Health>("/api/health"), 30000);
  const ok = health.data?.status === "ok";
  return (
    <div className="border-t border-edge px-5 py-3.5">
      <p className="flex items-center gap-2 text-[11px] text-faint">
        <span
          className={`size-1.5 rounded-full ${ok ? "bg-emerald-400" : "bg-rose-400"}`}
        />
        {ok ? `v${health.data?.version}` : "unreachable"}
      </p>
    </div>
  );
}
