import { useId } from 'react'

const control =
  'h-9 rounded-md border border-border bg-card px-3 text-sm focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary'

/** A labelled text filter; the value is committed on every change. */
export function TextFilter({
  label,
  value,
  onChange,
  placeholder,
  help,
}: {
  label: string
  value: string
  onChange: (v: string) => void
  placeholder?: string
  /** Inline error text; marks the input invalid. */
  help?: string
}) {
  const id = useId()
  return (
    <div className="flex flex-col gap-1">
      <label htmlFor={id} className="text-xs font-medium text-muted-foreground">
        {label}
      </label>
      <input
        id={id}
        className={control}
        value={value}
        placeholder={placeholder}
        aria-invalid={help ? true : undefined}
        aria-describedby={help ? `${id}-help` : undefined}
        onChange={(e) => onChange(e.target.value)}
      />
      {help && (
        <p id={`${id}-help`} className="max-w-64 text-xs text-destructive">
          {help}
        </p>
      )}
    </div>
  )
}

/** A labelled local date and time (`datetime-local`), in the browser's time zone; empty means no bound. */
export function DateTimeFilter({
  label,
  value,
  onChange,
  help,
}: {
  label: string
  value: string
  onChange: (v: string) => void
  /** Inline error text; marks the input invalid. */
  help?: string
}) {
  const id = useId()
  return (
    <div className="flex flex-col gap-1">
      <label htmlFor={id} className="text-xs font-medium text-muted-foreground">
        {label}
      </label>
      <input
        id={id}
        type="datetime-local"
        className={control}
        value={value}
        aria-invalid={help ? true : undefined}
        aria-describedby={help ? `${id}-help` : undefined}
        onChange={(e) => onChange(e.target.value)}
      />
      {help && (
        <p id={`${id}-help`} className="max-w-64 text-xs text-destructive">
          {help}
        </p>
      )}
    </div>
  )
}

export function SelectFilter({
  label,
  value,
  onChange,
  options,
}: {
  label: string
  value: string
  onChange: (v: string) => void
  options: { value: string; label: string }[]
}) {
  const id = useId()
  return (
    <div className="flex flex-col gap-1">
      <label htmlFor={id} className="text-xs font-medium text-muted-foreground">
        {label}
      </label>
      <select id={id} className={control} value={value} onChange={(e) => onChange(e.target.value)}>
        {options.map((o) => (
          <option key={o.value} value={o.value}>
            {o.label}
          </option>
        ))}
      </select>
    </div>
  )
}

export function FilterBar({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <form role="search" aria-label={label} className="mb-4 flex flex-wrap items-end gap-3" onSubmit={(e) => e.preventDefault()}>
      {children}
    </form>
  )
}
