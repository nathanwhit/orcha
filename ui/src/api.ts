// Typed client for the orcha HTTP API. Field names mirror the Go structs'
// JSON tags exactly.

export interface DashboardObjective {
  id: string;
  status: string;
  title: string;
  repo: string;
  active_sessions: number;
  pr_count: number;
  open_questions: number;
  needs_user: boolean;
  latest_activity: string;
  updated_at: string;
}

export interface Objective {
  id: string;
  title: string;
  prompt: string;
  status: string;
  manager_session_id?: string;
  created_at: string;
  updated_at: string;
  completed_at?: string;
  summary?: string;
  metadata?: Record<string, unknown>;
}

export interface DashboardSession {
  id: string;
  objective_id: string;
  status: string;
  role: string;
  agent: string;
  mode: string;
  title: string;
  target_id: string;
  current_activity: string;
  used_tokens: number;
  updated_at: string;
}

export interface Session {
  id: string;
  objective_id?: string;
  parent_session_id?: string;
  role: string;
  agent: string;
  mode: string;
  status: string;
  title: string;
  goal: string;
  current_activity?: string;
  latest_summary?: string;
  target_id?: string;
  workspace_id?: string;
  used_tokens: number;
  created_at: string;
  started_at?: string;
  updated_at: string;
  completed_at?: string;
  metadata?: Record<string, unknown>;
}

export interface Message {
  id: string;
  session_id: string;
  seq: number;
  source: string;
  kind: string;
  content: string;
  metadata?: Record<string, unknown>;
  created_at: string;
}

export interface Target {
  id: string;
  name: string;
  kind: string;
  status: string;
  host?: string;
  user?: string;
  work_root: string;
  labels?: string[];
  capacity_sessions: number;
  available_sessions: number;
  cpu_summary?: string;
  memory_summary?: string;
  disk_summary?: string;
  last_seen_at?: string;
  metadata?: Record<string, unknown>;
}

export interface DoctorCheck {
  name: string;
  ok: boolean;
  required: boolean;
  detail?: string;
}

export interface DoctorReport {
  target_id: string;
  target: string;
  ok: boolean;
  missing: string[];
  checks: DoctorCheck[];
}

export interface PullRequest {
  id: string;
  objective_id?: string;
  created_by_session_id?: string;
  repo: string;
  number: number;
  url: string;
  branch: string;
  base_branch: string;
  head_sha?: string;
  status: string;
  checks_state: string;
  title: string;
  summary?: string;
  last_synced_at?: string;
  created_at: string;
  updated_at: string;
}

export interface Question {
  id: string;
  objective_id?: string;
  session_id?: string;
  status: string;
  priority: number;
  question: string;
  context?: string;
  answer?: string;
  created_at: string;
}

export interface Artifact {
  id: string;
  objective_id?: string;
  session_id?: string;
  kind: string;
  title: string;
  summary?: string;
  uri?: string;
  visibility: string;
  created_at: string;
}

export interface Usage {
  id: string;
  provider: string;
  account: string;
  used_tokens: number;
  used_percent?: number;
  state: string;
  updated_at: string;
}

export interface OrchaEvent {
  id: number;
  objective_id?: string;
  session_id?: string;
  type: string;
  summary: string;
  data?: Record<string, unknown>;
  created_at: string;
}

export interface Project {
  id: string;
  name: string;
  repo: string;
  push_repo?: string;
  clone_url?: string;
  base_branch?: string;
  created_at: string;
  updated_at: string;
}

export interface Health {
  status: string;
  version: string;
  build: string;
  started: string;
  time: string;
}

export interface SessionUsageBreakdown {
  session_id: string;
  title: string;
  role: string;
  provider: string;
  used_tokens: number;
}

export interface ProviderUsageTotal {
  provider: string;
  used_tokens: number;
}

export interface ObjectiveUsageSummary {
  objective_id: string;
  total_tokens: number;
  sessions: SessionUsageBreakdown[];
  providers: ProviderUsageTotal[];
}

export interface ObjectiveDetail {
  objective: Objective;
  sessions: DashboardSession[];
  pull_requests: PullRequest[];
  questions: Question[];
  artifacts: Artifact[];
  usage?: ObjectiveUsageSummary;
}

// Compact token formatter, e.g. 1234 -> "1.2k", 2_500_000 -> "2.5M".
export function formatTokens(n: number): string {
  if (n < 1000) return String(n);
  if (n < 1_000_000) {
    const k = n / 1000;
    return `${k >= 100 ? Math.round(k) : k.toFixed(1)}k`;
  }
  const m = n / 1_000_000;
  return `${m >= 100 ? Math.round(m) : m.toFixed(1)}M`;
}

export interface Screen {
  screen: string;
  cols: number;
  rows: number;
  attach: string;
}

// Whoami reports the exe.dev-authenticated identity. Empty email means auth is
// not enabled (local dev), so the UI omits the identity affordance.
export interface Whoami {
  email: string;
  userId: string;
}

async function parse(r: Response) {
  if (r.status === 204) return null;
  const body = await r.json().catch(() => null);
  if (!r.ok) {
    const msg =
      body && typeof body.error === "string" ? body.error : r.statusText;
    throw new Error(msg);
  }
  return body;
}

export function get<T>(url: string): Promise<T> {
  return fetch(url).then(parse);
}

export function post<T>(url: string, body?: unknown): Promise<T> {
  return fetch(url, {
    method: "POST",
    headers: body ? { "Content-Type": "application/json" } : undefined,
    body: body ? JSON.stringify(body) : undefined,
  }).then(parse);
}

export function put<T>(url: string, body?: unknown): Promise<T> {
  return fetch(url, {
    method: "PUT",
    headers: body ? { "Content-Type": "application/json" } : undefined,
    body: body ? JSON.stringify(body) : undefined,
  }).then(parse);
}

export function patch<T>(url: string, body?: unknown): Promise<T> {
  return fetch(url, {
    method: "PATCH",
    headers: body ? { "Content-Type": "application/json" } : undefined,
    body: body ? JSON.stringify(body) : undefined,
  }).then(parse);
}

export function del<T>(url: string): Promise<T> {
  return fetch(url, { method: "DELETE" }).then(parse);
}

export const shortId = (id?: string) => (id ? id.slice(0, 8) : "");
