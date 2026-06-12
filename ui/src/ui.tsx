import { useEffect, useState } from "react";
import type { ReactNode, FormEvent } from "react";
import { Icon } from "./icons";
import { timeAgo, useNow } from "./hooks";

// ---- status pills ----

type Tone = "green" | "amber" | "orange" | "blue" | "violet" | "rose" | "zinc";

const TONE_FOR_STATUS: Record<string, Tone> = {
  running: "green",
  active: "green",
  online: "green",
  open: "green",
  passing: "green",
  ready: "green",
  queued: "amber",
  starting: "amber",
  pending: "amber",
  preparing: "amber",
  waiting_capacity: "amber",
  draining: "amber",
  waiting_user: "orange",
  succeeded: "blue",
  answered: "blue",
  merged: "violet",
  failed: "rose",
  failing: "rose",
  error: "rose",
  offline: "rose",
  canceled: "zinc",
  closed: "zinc",
  disabled: "zinc",
  unknown: "zinc",
  draft: "zinc",
};

const TONE_CLASSES: Record<Tone, string> = {
  green: "bg-emerald-400/10 text-emerald-300 ring-emerald-400/25",
  amber: "bg-amber-400/10 text-amber-300 ring-amber-400/25",
  orange: "bg-orange-400/10 text-orange-300 ring-orange-400/30",
  blue: "bg-sky-400/10 text-sky-300 ring-sky-400/25",
  violet: "bg-violet-400/10 text-violet-300 ring-violet-400/25",
  rose: "bg-rose-400/10 text-rose-300 ring-rose-400/25",
  zinc: "bg-zinc-400/10 text-zinc-400 ring-zinc-400/20",
};

const PULSING = new Set(["running", "starting", "waiting_user", "preparing"]);

export function Pill({ status }: { status: string }) {
  const tone = TONE_FOR_STATUS[status] ?? "zinc";
  return (
    <span
      className={`inline-flex items-center gap-1.5 rounded-full px-2 py-0.5 text-[11px] font-medium ring-1 ring-inset whitespace-nowrap ${TONE_CLASSES[tone]}`}
    >
      <span
        className={`size-1.5 rounded-full bg-current ${PULSING.has(status) ? "animate-pulse" : ""}`}
      />
      {status.replaceAll("_", " ")}
    </span>
  );
}

export function Chip({ children }: { children: ReactNode }) {
  return (
    <span className="inline-flex items-center rounded-md bg-raised px-1.5 py-0.5 text-[11px] text-mute ring-1 ring-inset ring-edge whitespace-nowrap">
      {children}
    </span>
  );
}

// ---- layout primitives ----

export function Card({
  children,
  className = "",
}: {
  children: ReactNode;
  className?: string;
}) {
  return (
    <div
      className={`rounded-xl border border-edge bg-surface ${className}`}
    >
      {children}
    </div>
  );
}

export function SectionTitle({
  children,
  right,
}: {
  children: ReactNode;
  right?: ReactNode;
}) {
  return (
    <div className="mb-3 flex items-center justify-between gap-3">
      <h2 className="text-[11px] font-semibold tracking-[0.14em] text-faint uppercase">
        {children}
      </h2>
      {right}
    </div>
  );
}

export function EmptyState({ children }: { children: ReactNode }) {
  return (
    <div className="px-4 py-8 text-center text-sm text-faint">{children}</div>
  );
}

// Collapsible is a section that hides its contents behind a clickable header,
// collapsed by default. Used for low-priority groups (e.g. canceled items).
export function Collapsible({
  title,
  count,
  defaultOpen = false,
  children,
}: {
  title: ReactNode;
  count?: number;
  defaultOpen?: boolean;
  children: ReactNode;
}) {
  const [open, setOpen] = useState(defaultOpen);
  return (
    <div>
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className="mb-3 flex w-full items-center gap-2 text-left text-[11px] font-semibold tracking-[0.14em] text-faint uppercase transition-colors hover:text-mute"
      >
        <Icon
          name="chevron"
          className={`size-3.5 transition-transform ${open ? "" : "-rotate-90"}`}
        />
        {title}
        {count !== undefined && (
          <span className="tabular-nums text-faint/70">{count}</span>
        )}
      </button>
      {open && children}
    </div>
  );
}

// ---- buttons ----

const BUTTON_VARIANTS = {
  primary:
    "bg-accent text-[#0a0d13] hover:bg-[#92a5ff] font-semibold shadow-[0_0_20px_-6px_var(--color-accent)]",
  ghost:
    "text-mute hover:text-ink ring-1 ring-inset ring-edge hover:ring-edge-bright hover:bg-raised",
  danger:
    "text-rose-300 ring-1 ring-inset ring-rose-400/25 hover:bg-rose-400/10",
} as const;

export function Button({
  children,
  onClick,
  variant = "ghost",
  type = "button",
  disabled,
  className = "",
}: {
  children: ReactNode;
  onClick?: () => void;
  variant?: keyof typeof BUTTON_VARIANTS;
  type?: "button" | "submit";
  disabled?: boolean;
  className?: string;
}) {
  return (
    <button
      type={type}
      onClick={onClick}
      disabled={disabled}
      className={`inline-flex items-center gap-1.5 rounded-lg px-3 py-1.5 text-[13px] transition-colors disabled:cursor-not-allowed disabled:opacity-40 ${BUTTON_VARIANTS[variant]} ${className}`}
    >
      {children}
    </button>
  );
}

export function IconButton({
  name,
  title,
  onClick,
  disabled,
}: {
  name: Parameters<typeof Icon>[0]["name"];
  title: string;
  onClick?: () => void;
  disabled?: boolean;
}) {
  return (
    <button
      type="button"
      title={title}
      onClick={onClick}
      disabled={disabled}
      className="rounded-md p-1.5 text-mute transition-colors hover:bg-raised hover:text-ink disabled:opacity-40"
    >
      <Icon name={name} />
    </button>
  );
}

// ---- forms ----

const FIELD_CLASS =
  "w-full rounded-lg border border-edge bg-raised px-3 py-2 text-[13px] text-ink placeholder:text-faint outline-none transition-colors focus:border-accent/60 focus:ring-2 focus:ring-accent/20";

export function Field({
  label,
  children,
  hint,
}: {
  label: string;
  children: ReactNode;
  hint?: string;
}) {
  return (
    <label className="block">
      <span className="mb-1.5 flex items-baseline justify-between text-xs font-medium text-mute">
        {label}
        {hint && <span className="text-[11px] text-faint">{hint}</span>}
      </span>
      {children}
    </label>
  );
}

export function TextInput(props: React.InputHTMLAttributes<HTMLInputElement>) {
  return <input {...props} className={FIELD_CLASS} />;
}

export function TextArea(
  props: React.TextareaHTMLAttributes<HTMLTextAreaElement>,
) {
  return <textarea {...props} className={`${FIELD_CLASS} resize-y`} />;
}

export function Select(props: React.SelectHTMLAttributes<HTMLSelectElement>) {
  return <select {...props} className={FIELD_CLASS} />;
}

// ---- modal ----

export function Modal({
  title,
  onClose,
  children,
}: {
  title: string;
  onClose: () => void;
  children: ReactNode;
}) {
  useEffect(() => {
    const on = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    window.addEventListener("keydown", on);
    return () => window.removeEventListener("keydown", on);
  }, [onClose]);
  return (
    <div
      className="fixed inset-0 z-50 flex items-start justify-center overflow-y-auto bg-black/60 p-4 pt-[8vh] backdrop-blur-sm"
      onMouseDown={(e) => {
        if (e.target === e.currentTarget) onClose();
      }}
    >
      <div className="w-full max-w-lg rounded-2xl border border-edge bg-surface shadow-2xl shadow-black/60">
        <div className="flex items-center justify-between border-b border-edge px-5 py-3.5">
          <h3 className="text-sm font-semibold">{title}</h3>
          <IconButton name="x" title="Close" onClick={onClose} />
        </div>
        <div className="p-5">{children}</div>
      </div>
    </div>
  );
}

// ---- misc ----

export function TimeAgo({ iso }: { iso?: string | null }) {
  useNow();
  const rel = timeAgo(iso);
  if (!rel) return null;
  return (
    <span
      className="text-xs whitespace-nowrap text-faint"
      title={iso ? new Date(iso).toLocaleString() : undefined}
    >
      {rel}
    </span>
  );
}

export function CopyButton({ text }: { text: string }) {
  const [copied, setCopied] = useState(false);
  return (
    <button
      type="button"
      title="Copy"
      onClick={() => {
        void navigator.clipboard.writeText(text);
        setCopied(true);
        setTimeout(() => setCopied(false), 1200);
      }}
      className="rounded p-1 text-faint transition-colors hover:text-ink"
    >
      <Icon name={copied ? "check" : "copy"} className="size-3.5" />
    </button>
  );
}

// SteerBox is the shared "send a message to a session" composer.
export function SteerBox({
  placeholder,
  onSend,
}: {
  placeholder: string;
  onSend: (text: string) => Promise<void>;
}) {
  const [text, setText] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    const t = text.trim();
    if (!t || busy) return;
    setBusy(true);
    setErr(null);
    try {
      await onSend(t);
      setText("");
    } catch (ex) {
      setErr(ex instanceof Error ? ex.message : String(ex));
    } finally {
      setBusy(false);
    }
  };
  return (
    <form onSubmit={submit} className="flex flex-col gap-1">
      <div className="flex items-center gap-2">
        <input
          value={text}
          onChange={(e) => setText(e.target.value)}
          placeholder={placeholder}
          className={FIELD_CLASS}
        />
        <Button type="submit" variant="primary" disabled={busy || !text.trim()}>
          <Icon name="send" className="size-3.5" />
          Steer
        </Button>
      </div>
      {err && <p className="text-xs text-rose-400">{err}</p>}
    </form>
  );
}

// ---- table helpers ----

export function Th({ children }: { children?: ReactNode }) {
  return (
    <th className="px-4 py-2.5 text-left text-[11px] font-semibold tracking-wider text-faint uppercase">
      {children}
    </th>
  );
}

export function Td({
  children,
  className = "",
}: {
  children?: ReactNode;
  className?: string;
}) {
  return <td className={`px-4 py-2.5 align-middle ${className}`}>{children}</td>;
}
