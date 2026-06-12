// Minimal inline icon set (lucide-style strokes), one path per icon.

const PATHS = {
  grid: "M3 3h7v7H3z M14 3h7v7h-7z M3 14h7v7H3z M14 14h7v7h-7z",
  terminal: "m4 17 6-6-6-6 M12 19h8",
  server: "M2 4h20v7H2z M2 13h20v7H2z M6 7.5h.01 M6 16.5h.01",
  pr: "M6 8.5a3 3 0 1 0 0-6 3 3 0 0 0 0 6Z M6 8.5V21 M18 15.5a3 3 0 1 0 0 6 3 3 0 0 0 0-6Z M18 15.5V10a4 4 0 0 0-4-4h-3 M13.5 3.5 11 6l2.5 2.5",
  activity: "M22 12h-4l-3 9L9 3l-3 9H2",
  plus: "M12 5v14 M5 12h14",
  x: "M18 6 6 18 M6 6l12 12",
  send: "m22 2-7 20-4-9-9-4Z M22 2 11 13",
  refresh: "M21 12a9 9 0 1 1-2.6-6.4L21 8 M21 3v5h-5",
  external: "M15 3h6v6 M21 3l-9 9 M19 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V7a2 2 0 0 1 2-2h6",
  back: "m15 18-6-6 6-6",
  chevron: "m6 9 6 6 6-6",
  help: "M12 22a10 10 0 1 0 0-20 10 10 0 0 0 0 20Z M9.1 9a3 3 0 0 1 5.8 1c0 2-3 2.6-3 4 M12 17.5h.01",
  check: "M20 6 9 17l-5-5",
  alert: "M12 9v4 M12 17h.01 M10.3 3.9 1.8 18a2 2 0 0 0 1.7 3h17a2 2 0 0 0 1.7-3L13.7 3.9a2 2 0 0 0-3.4 0Z",
  copy: "M9 9h11v11H9z M15 9V4H4v11h5",
  stop: "M7 7h10v10H7z",
  play: "m7 5 12 7-12 7Z",
  menu: "M4 6h16 M4 12h16 M4 18h16",
  sun: "M12 4V2 M12 22v-2 M4.93 4.93 3.52 3.52 M20.48 20.48l-1.41-1.41 M4 12H2 M22 12h-2 M4.93 19.07l-1.41 1.41 M20.48 3.52l-1.41 1.41 M12 17a5 5 0 1 0 0-10 5 5 0 0 0 0 10Z",
  moon: "M20.9 13.1A8 8 0 0 1 10.9 3.1 7 7 0 1 0 20.9 13.1Z",
  target: "M12 22a10 10 0 1 0 0-20 10 10 0 0 0 0 20Z M12 16a4 4 0 1 0 0-8 4 4 0 0 0 0 8Z",
  stethoscope: "M5 3v6a5 5 0 0 0 10 0V3 M3 3h4 M13 3h4 M15 14v2a5 5 0 0 0 10 0v-1 M20 13.5a1.5 1.5 0 1 0 0-3 1.5 1.5 0 0 0 0 3Z",
  folder: "M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v10a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2Z",
} as const;

export type IconName = keyof typeof PATHS;

export function Icon({
  name,
  className = "size-4",
}: {
  name: IconName;
  className?: string;
}) {
  return (
    <svg
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.8"
      strokeLinecap="round"
      strokeLinejoin="round"
      className={className}
      aria-hidden="true"
    >
      <path d={PATHS[name]} />
    </svg>
  );
}
