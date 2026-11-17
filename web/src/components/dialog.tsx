import { useEffect, useId, useRef, type ReactNode } from 'react'

const FOCUSABLE = 'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])'

/**
 * A modal dialog: labelled, focus moves into it and is trapped (Tab cycles), focus returns to
 * the opener on close. Escape and a click on the backdrop close it only while `dismissible`.
 */
export function Dialog({
  title,
  onClose,
  dismissible = true,
  children,
}: {
  title: string
  onClose: () => void
  dismissible?: boolean
  children: ReactNode
}) {
  const titleId = useId()
  const ref = useRef<HTMLDivElement>(null)
  const closeRef = useRef({ onClose, dismissible })
  closeRef.current = { onClose, dismissible }

  useEffect(() => {
    const opener = document.activeElement as HTMLElement | null
    const first = ref.current?.querySelector<HTMLElement>(FOCUSABLE)
    ;(first ?? ref.current)?.focus()
    return () => {
      if (opener && opener.isConnected) opener.focus()
    }
  }, [])

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        if (closeRef.current.dismissible) closeRef.current.onClose()
        return
      }
      if (e.key !== 'Tab' || !ref.current) return
      const items = [...ref.current.querySelectorAll<HTMLElement>(FOCUSABLE)]
      if (items.length === 0) {
        e.preventDefault()
        ref.current.focus()
        return
      }
      const firstEl = items[0]!
      const lastEl = items[items.length - 1]!
      const active = document.activeElement
      if (e.shiftKey && (active === firstEl || active === ref.current || !ref.current.contains(active))) {
        e.preventDefault()
        lastEl.focus()
      } else if (!e.shiftKey && (active === lastEl || !ref.current.contains(active))) {
        e.preventDefault()
        firstEl.focus()
      }
    }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [])

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/50 p-4"
      onMouseDown={(e) => {
        if (e.target === e.currentTarget && dismissible) onClose()
      }}
    >
      <div
        ref={ref}
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        tabIndex={-1}
        className="max-h-full w-full max-w-lg overflow-auto rounded-[2px] border border-border bg-card p-6 shadow-lg"
      >
        <h2 id={titleId} className="mb-3 text-lg font-semibold">
          {title}
        </h2>
        {children}
      </div>
    </div>
  )
}
