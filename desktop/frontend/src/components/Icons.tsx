import type { JSX } from "react";

/**
 * Inline stroke icons.
 *
 * They are hand-written rather than pulled from an icon package: the app needs
 * six icons, and a dependency that ships thousands is a poor trade. All of them
 * inherit `currentColor` and share a 1.75 stroke so they sit together evenly.
 */

interface IconProps {
  size?: number;
  className?: string;
}

function base(size: number, className?: string) {
  return {
    width: size,
    height: size,
    viewBox: "0 0 24 24",
    fill: "none",
    stroke: "currentColor",
    strokeWidth: 1.75,
    strokeLinecap: "round" as const,
    strokeLinejoin: "round" as const,
    className,
    "aria-hidden": true,
    focusable: false,
  };
}

export function IconDownload({
  size = 18,
  className,
}: IconProps): JSX.Element {
  return (
    <svg {...base(size, className)}>
      <path d="M12 3v12" />
      <path d="m7 11 5 5 5-5" />
      <path d="M4 20h16" />
    </svg>
  );
}

export function IconInbox({ size = 18, className }: IconProps): JSX.Element {
  return (
    <svg {...base(size, className)}>
      <path d="M3 12h5l2 3h4l2-3h5" />
      <path d="M5 5h14l2 7v5a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-5Z" />
    </svg>
  );
}

export function IconCheck({ size = 18, className }: IconProps): JSX.Element {
  return (
    <svg {...base(size, className)}>
      <circle cx="12" cy="12" r="9" />
      <path d="m8.5 12.5 2.5 2.5 4.5-5" />
    </svg>
  );
}

export function IconAlert({ size = 18, className }: IconProps): JSX.Element {
  return (
    <svg {...base(size, className)}>
      <path d="M12 4.5 3.5 19h17Z" />
      <path d="M12 10v4" />
      <path d="M12 17h.01" />
    </svg>
  );
}

export function IconPause({ size = 18, className }: IconProps): JSX.Element {
  return (
    <svg {...base(size, className)}>
      <rect x="7" y="5" width="3.5" height="14" rx="1" />
      <rect x="13.5" y="5" width="3.5" height="14" rx="1" />
    </svg>
  );
}

export function IconPlay({ size = 18, className }: IconProps): JSX.Element {
  return (
    <svg {...base(size, className)}>
      <path d="M8 5.5v13l10-6.5Z" />
    </svg>
  );
}

export function IconX({ size = 18, className }: IconProps): JSX.Element {
  return (
    <svg {...base(size, className)}>
      <path d="M6 6l12 12M18 6 6 18" />
    </svg>
  );
}

export function IconFolder({ size = 18, className }: IconProps): JSX.Element {
  return (
    <svg {...base(size, className)}>
      <path d="M3 7a2 2 0 0 1 2-2h4l2 2.5h8a2 2 0 0 1 2 2V17a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2Z" />
    </svg>
  );
}

export function IconSettings({ size = 18, className }: IconProps): JSX.Element {
  return (
    <svg {...base(size, className)}>
      <circle cx="12" cy="12" r="3" />
      <path d="M12 3v2.5M12 18.5V21M4.2 7.5l2.2 1.3M17.6 15.2l2.2 1.3M4.2 16.5l2.2-1.3M17.6 8.8l2.2-1.3" />
    </svg>
  );
}

export function IconPlus({ size = 18, className }: IconProps): JSX.Element {
  return (
    <svg {...base(size, className)}>
      <path d="M12 5v14M5 12h14" />
    </svg>
  );
}

export function IconTrash({ size = 18, className }: IconProps): JSX.Element {
  return (
    <svg {...base(size, className)}>
      <path d="M4 7h16" />
      <path d="M9 7V5h6v2" />
      <path d="M6 7l1 12a1 1 0 0 0 1 1h8a1 1 0 0 0 1-1l1-12" />
      <path d="M10 11v6M14 11v6" />
    </svg>
  );
}
