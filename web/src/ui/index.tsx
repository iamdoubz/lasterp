// SPDX-License-Identifier: AGPL-3.0-only

// LastERP UI Kit foundations (ADR-010). shadcn-style: components are copied in
// and owned here, not pulled from a component dependency.
//
// Two rules hold across every component and are not negotiable per-screen:
//   - Logical CSS properties only (ms-/me-/ps-/pe-/text-start), never left/right.
//     Direction comes from the document `dir` attribute (docs/17), so an RTL
//     locale needs no component changes.
//   - Keyboard operable with a visible focus ring. Anything clickable is a real
//     button or anchor element; nothing interactive is a bare div.

import type {
  ButtonHTMLAttributes,
  InputHTMLAttributes,
  ReactNode,
  SelectHTMLAttributes,
  TextareaHTMLAttributes,
} from "react";

/** Shared focus treatment. Never remove the ring — WCAG 2.2 AA (docs/17). */
const focusRing =
  "focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-sky-600";

const controlBase =
  "block w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-start text-slate-900 " +
  "placeholder:text-slate-400 disabled:cursor-not-allowed disabled:bg-slate-100 " +
  "dark:border-slate-600 dark:bg-slate-800 dark:text-slate-100 " +
  focusRing;

function cx(...parts: (string | false | undefined)[]): string {
  return parts.filter(Boolean).join(" ");
}

// --- Button ---

export type ButtonVariant = "primary" | "secondary" | "danger";

const buttonVariants: Record<ButtonVariant, string> = {
  primary: "bg-sky-700 text-white hover:bg-sky-800 disabled:bg-sky-300",
  secondary:
    "bg-white text-slate-900 border border-slate-300 hover:bg-slate-50 " +
    "dark:bg-slate-800 dark:text-slate-100 dark:border-slate-600 dark:hover:bg-slate-700",
  danger: "bg-red-700 text-white hover:bg-red-800 disabled:bg-red-300",
};

/**
 * buttonClass is the button look as classes, for the cases where the element
 * must be an anchor — a navigation target is a link, not a button, and nesting
 * a Button inside a Link produces a zero-height anchor that fails WCAG 2.2
 * target-size (2.5.8). Style the link itself; never nest the two.
 *
 * min-h/min-w carry the 24×24 minimum target size explicitly rather than
 * relying on padding to happen to reach it.
 */
export function buttonClass(variant: ButtonVariant = "secondary", className?: string): string {
  return cx(
    "inline-flex min-h-11 min-w-11 items-center justify-center rounded-md px-3 py-2 text-sm font-medium no-underline",
    "disabled:cursor-not-allowed",
    focusRing,
    buttonVariants[variant],
    className,
  );
}

type ButtonProps = ButtonHTMLAttributes<HTMLButtonElement> & {
  variant?: ButtonVariant;
};

export function Button({ variant = "secondary", className, type, ...props }: ButtonProps) {
  return (
    <button
      // An explicit type is not a nicety: a button inside a form defaults to
      // submit, so an unmarked secondary action silently submits the form.
      type={type ?? "button"}
      className={buttonClass(variant, className)}
      {...props}
    />
  );
}

// --- form controls ---

export function Input({ className, ...props }: InputHTMLAttributes<HTMLInputElement>) {
  return <input className={cx(controlBase, className)} {...props} />;
}

export function Textarea({ className, ...props }: TextareaHTMLAttributes<HTMLTextAreaElement>) {
  return <textarea rows={4} className={cx(controlBase, className)} {...props} />;
}

export function Select({ className, ...props }: SelectHTMLAttributes<HTMLSelectElement>) {
  return <select className={cx(controlBase, className)} {...props} />;
}

export function Checkbox({ className, ...props }: InputHTMLAttributes<HTMLInputElement>) {
  return (
    <input
      type="checkbox"
      className={cx(
        "size-4 rounded border-slate-300 text-sky-700 dark:border-slate-600",
        focusRing,
        className,
      )}
      {...props}
    />
  );
}

interface RadioProps extends InputHTMLAttributes<HTMLInputElement> {
  /** The visible text for this option. The input owns its own label because a
   * radio group's outer label names the question, not the answers. */
  label: string;
}

export function Radio({ className, label, id, ...props }: RadioProps) {
  return (
    <span className="flex items-center gap-2">
      <input
        type="radio"
        id={id}
        className={cx("size-4 border-slate-300 text-sky-700 dark:border-slate-600", focusRing, className)}
        {...props}
      />
      <label htmlFor={id} className="text-sm text-slate-700 dark:text-slate-200">
        {label}
      </label>
    </span>
  );
}

// --- Field: label + control + error, wired for screen readers ---

interface FieldProps {
  id: string;
  label: string;
  required?: boolean;
  error?: string;
  description?: string;
  children: ReactNode;
}

/**
 * Field owns the label/description/error wiring so no screen has to remember
 * it. The control it wraps must carry `id`, and gets described by whichever of
 * description/error is present via aria-describedby.
 */
export function Field({ id, label, required, error, description, children }: FieldProps) {
  const describedBy = [description && `${id}-description`, error && `${id}-error`]
    .filter(Boolean)
    .join(" ");

  return (
    <div className="mb-4">
      <label
        htmlFor={id}
        // The id lets a composite control (a radio group, which has no single
        // focusable element for htmlFor to point at) name itself with
        // aria-labelledby.
        id={`${id}-label`}
        className="mb-1 block text-sm font-medium text-slate-700 dark:text-slate-200"
      >
        {label}
        {required && (
          <span aria-hidden="true" className="ms-1 text-red-700">
            *
          </span>
        )}
      </label>
      <div
        // The control is cloned by the caller with these ids; passing them down
        // via context would hide the wiring that makes this accessible.
        data-describedby={describedBy || undefined}
      >
        {children}
      </div>
      {description && (
        <p id={`${id}-description`} className="mt-1 text-sm text-slate-500 dark:text-slate-400">
          {description}
        </p>
      )}
      {error && (
        <p id={`${id}-error`} role="alert" className="mt-1 text-sm text-red-700 dark:text-red-400">
          {error}
        </p>
      )}
    </div>
  );
}

// --- Table ---

interface TableProps {
  caption: string;
  columns: string[];
  children: ReactNode;
}

/**
 * A semantic data table. No sorting, column config, or virtualization in v1 —
 * see WP-1.5-decisions.md §6 for why TanStack Table waits for a screen that
 * needs it. The caption is required, not optional: a screen-reader user
 * arriving at an uncaptioned table has no idea what it lists.
 */
export function Table({ caption, columns, children }: TableProps) {
  return (
    <div className="overflow-x-auto">
      <table className="w-full border-collapse text-sm">
        <caption className="mb-2 text-start text-base font-semibold text-slate-900 dark:text-slate-100">
          {caption}
        </caption>
        <thead>
          <tr className="border-b border-slate-300 dark:border-slate-600">
            {columns.map((c) => (
              <th key={c} scope="col" className="px-3 py-2 text-start font-medium text-slate-700 dark:text-slate-200">
                {c}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>{children}</tbody>
      </table>
    </div>
  );
}

// --- status / feedback ---

/**
 * Alert renders a server problem or a local message. Errors get role="alert"
 * so assistive tech announces them without the user hunting for what changed.
 */
export function Alert({ tone = "error", children }: { tone?: "error" | "info"; children: ReactNode }) {
  const tones = {
    error: "border-red-300 bg-red-50 text-red-900 dark:border-red-800 dark:bg-red-950 dark:text-red-100",
    info: "border-sky-300 bg-sky-50 text-sky-900 dark:border-sky-800 dark:bg-sky-950 dark:text-sky-100",
  };
  return (
    <div
      role={tone === "error" ? "alert" : "status"}
      className={cx("mb-4 rounded-md border px-3 py-2 text-sm", tones[tone])}
    >
      {children}
    </div>
  );
}

/** Busy is the loading state. aria-busy + a live region means a screen reader
 * hears "loading" instead of silence. */
export function Busy({ label }: { label: string }) {
  return (
    <p role="status" aria-busy="true" className="py-4 text-sm text-slate-500 dark:text-slate-400">
      {label}
    </p>
  );
}
