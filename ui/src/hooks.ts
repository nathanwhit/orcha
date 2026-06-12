import { useCallback, useEffect, useRef, useState } from "react";
import type { DependencyList } from "react";

// usePoll repeatedly invokes fn (immediately, then every ms after each
// completion) and exposes the latest result. Errors are kept separately so
// stale-but-valid data stays on screen during a blip.
export function usePoll<T>(
  fn: () => Promise<T>,
  ms: number,
  deps: DependencyList = [],
) {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<string | null>(null);
  const fnRef = useRef(fn);
  fnRef.current = fn;
  const [bump, setBump] = useState(0);

  useEffect(() => {
    let live = true;
    let timer: number | undefined;
    const run = async () => {
      try {
        const d = await fnRef.current();
        if (live) {
          setData(d);
          setError(null);
        }
      } catch (e) {
        if (live) setError(e instanceof Error ? e.message : String(e));
      }
      if (live) timer = window.setTimeout(run, ms);
    };
    void run();
    return () => {
      live = false;
      if (timer !== undefined) clearTimeout(timer);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [ms, bump, ...deps]);

  const reload = useCallback(() => setBump((b) => b + 1), []);
  return { data, error, reload };
}

// Minimal hash router: "#/sessions/abc" -> "/sessions/abc". Hash routing means
// the Go server only ever serves real files — no SPA fallback needed.
export function useHashPath(): [string, (to: string) => void] {
  const read = () => window.location.hash.replace(/^#/, "") || "/";
  const [path, setPath] = useState(read);
  useEffect(() => {
    const on = () => setPath(read());
    window.addEventListener("hashchange", on);
    return () => window.removeEventListener("hashchange", on);
  }, []);
  const nav = useCallback((to: string) => {
    window.location.hash = to;
  }, []);
  return [path, nav];
}

export function timeAgo(iso?: string | null): string {
  if (!iso) return "";
  const t = new Date(iso).getTime();
  if (!Number.isFinite(t) || t <= 0) return "";
  const s = Math.floor((Date.now() - t) / 1000);
  if (s < 5) return "now";
  if (s < 60) return `${s}s ago`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ago`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ago`;
  const d = Math.floor(h / 24);
  if (d < 30) return `${d}d ago`;
  return new Date(t).toLocaleDateString();
}

// useNow re-renders periodically so relative timestamps stay fresh.
export function useNow(ms = 15000): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const t = window.setInterval(() => setNow(Date.now()), ms);
    return () => clearInterval(t);
  }, [ms]);
  return now;
}
