import { Component, useEffect, useRef, useState } from "react";
import type { ReactNode } from "react";
import * as api from "./api";
import { useHashPath, usePoll } from "./hooks";
import { IconButton } from "./ui";
import { Icon } from "./icons";
import type { IconName } from "./icons";
import { ActivityPage } from "./pages/Activity";
import { ObjectivePage } from "./pages/Objective";
import { Overview } from "./pages/Overview";
import { ProjectsPage } from "./pages/Projects";
import { PullRequestsPage } from "./pages/PullRequests";
import { SessionPage } from "./pages/SessionDetail";
import { SessionsPage } from "./pages/Sessions";
import { TargetsPage } from "./pages/Targets";

const NAV: { to: string; label: string; icon: IconName }[] = [
  { to: "/", label: "Overview", icon: "grid" },
  { to: "/sessions", label: "Sessions", icon: "terminal" },
  { to: "/prs", label: "Pull requests", icon: "pr" },
  { to: "/projects", label: "Projects", icon: "folder" },
  { to: "/targets", label: "Targets", icon: "server" },
  { to: "/activity", label: "Activity", icon: "activity" },
];

type Theme = "dark" | "light";

const THEME_KEY = "orcha.theme";

function applyTheme(theme: Theme) {
  if (theme === "light") {
    document.documentElement.dataset.theme = "light";
  } else {
    delete document.documentElement.dataset.theme;
  }
}

function readInitialTheme(): Theme {
  let theme: Theme = "dark";
  try {
    theme = localStorage.getItem(THEME_KEY) === "light" ? "light" : "dark";
  } catch {
    // Keep the CSS default if storage is unavailable.
  }
  applyTheme(theme);
  return theme;
}

function storeTheme(theme: Theme) {
  try {
    localStorage.setItem(THEME_KEY, theme);
  } catch {
    // Theme changes should still work for the current page.
  }
}

export default function App() {
  const [path, nav] = useHashPath();
  const [menuOpen, setMenuOpen] = useState(false);
  const [theme, setTheme] = useState<Theme>(readInitialTheme);
  const seg = path.split("/").filter(Boolean);

  useEffect(() => {
    applyTheme(theme);
  }, [theme]);

  const toggleTheme = () => {
    setTheme((current) => {
      const next = current === "light" ? "dark" : "light";
      storeTheme(next);
      return next;
    });
  };

  let page: React.ReactNode;
  if (seg.length === 0) page = <Overview nav={nav} />;
  else if (seg[0] === "objectives" && seg[1])
    page = <ObjectivePage id={seg[1]} nav={nav} />;
  else if (seg[0] === "sessions" && seg[1]) page = <SessionPage id={seg[1]} />;
  else if (seg[0] === "sessions") page = <SessionsPage nav={nav} />;
  else if (seg[0] === "prs") page = <PullRequestsPage />;
  else if (seg[0] === "projects") page = <ProjectsPage />;
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
        <div className="flex items-center gap-1">
          <ThemeToggle
            theme={theme}
            onToggle={toggleTheme}
          />
          <button
            type="button"
            onClick={() => setMenuOpen((v) => !v)}
            className="rounded-md p-1.5 text-mute transition-colors hover:bg-raised hover:text-ink"
          >
            <Icon name={menuOpen ? "x" : "menu"} className="size-5" />
          </button>
        </div>
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
            <div className="mt-2">
              <HealthFooter />
            </div>
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
        <div className="border-t border-edge px-3 py-2">
          <ThemeToggle
            theme={theme}
            showLabel
            onToggle={toggleTheme}
          />
        </div>
        <HealthFooter />
      </aside>

      <main className="mx-auto w-full max-w-6xl px-4 py-6 md:px-8 md:py-8">
        {/* Keyed by path so navigating away clears a crashed page. */}
        <ErrorBoundary key={path}>{page}</ErrorBoundary>
      </main>
    </div>
  );
}

function ThemeToggle({
  theme,
  onToggle,
  showLabel = false,
}: {
  theme: Theme;
  onToggle: () => void;
  showLabel?: boolean;
}) {
  const next = theme === "light" ? "dark" : "light";
  return (
    <button
      type="button"
      title={`Switch to ${next} mode`}
      aria-label={`Switch to ${next} mode`}
      onClick={onToggle}
      className={`rounded-md text-mute transition-colors hover:bg-raised hover:text-ink ${
        showLabel
          ? "flex w-full items-center gap-2.5 px-2.5 py-2 text-[13px]"
          : "p-1.5"
      }`}
    >
      <Icon name={theme === "light" ? "moon" : "sun"} className="size-4" />
      {showLabel && <span>{theme === "light" ? "Dark mode" : "Light mode"}</span>}
    </button>
  );
}

// ErrorBoundary turns a render crash into a readable error instead of
// unmounting the whole app (a black page).
class ErrorBoundary extends Component<
  { children: ReactNode },
  { error: Error | null }
> {
  state = { error: null as Error | null };
  static getDerivedStateFromError(error: Error) {
    return { error };
  }
  render() {
    if (this.state.error) {
      return (
        <div className="rounded-xl border border-rose-400/25 bg-rose-400/5 p-5">
          <p className="text-sm font-semibold text-rose-300">
            This page crashed.
          </p>
          <pre className="mt-2 overflow-x-auto font-mono text-xs whitespace-pre-wrap text-mute">
            {this.state.error.message}
          </pre>
          <a href="#/" className="mt-3 inline-block text-sm text-accent">
            Back to overview
          </a>
        </div>
      );
    }
    return this.props.children;
  }
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
  const health = usePoll(() => api.get<api.Health>("/api/health"), 10000);
  const [restarting, setRestarting] = useState(false);
  const [me, setMe] = useState<api.Whoami | null>(null);
  useEffect(() => {
    void api
      .get<api.Whoami>("/api/whoami")
      .then(setMe)
      .catch(() => {});
  }, []);
  const startedRef = useRef<string | undefined>(undefined);
  const started = health.data?.started;
  // A new `started` timestamp means the restarted process is up.
  useEffect(() => {
    if (started && startedRef.current && started !== startedRef.current)
      setRestarting(false);
    if (started) startedRef.current = started;
  }, [started]);

  const ok = health.data?.status === "ok";
  return (
    <div className="border-t border-edge px-3 py-2">
      {me?.email && (
        <div className="mb-1 flex items-center justify-between gap-2 px-2 text-[11px] text-faint">
          <span className="truncate" title={me.email}>
            {me.email}
          </span>
          <button
            className="shrink-0 text-mute hover:text-fg"
            title="Sign out of exe.dev"
            onClick={() => {
              void fetch("/__exe.dev/logout", { method: "POST" }).finally(() => {
                window.location.assign("/");
              });
            }}
          >
            Logout
          </button>
        </div>
      )}
      <div className="flex items-center justify-between">
        <p
          className="flex items-center gap-2 px-2 text-[11px] text-faint"
          title={health.data ? `build ${health.data.build} · started ${health.data.started}` : undefined}
        >
          <span
            className={`size-1.5 rounded-full ${restarting ? "animate-pulse bg-amber-400" : ok ? "bg-emerald-400" : "bg-rose-400"}`}
          />
          {restarting ? "restarting…" : ok ? `v${health.data?.version}` : "unreachable"}
        </p>
        <IconButton
          name="refresh"
          title="Pull latest, rebuild & restart orcha (needs scripts/dev.sh supervising)"
          disabled={restarting}
          onClick={() => {
            if (!confirm("Pull latest, rebuild, and restart the orcha server? Live tmux sessions are re-adopted."))
              return;
            setRestarting(true);
            void api.post("/api/restart").catch(() => setRestarting(false));
          }}
        />
      </div>
    </div>
  );
}
