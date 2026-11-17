import * as React from 'react'
import { cva, type VariantProps } from 'class-variance-authority'
import { cn } from '@/lib/utils'

/** Square slip labels in mono: the wire's data voice. */
const badgeVariants = cva(
  'inline-flex items-center rounded-[2px] border px-1.5 py-px font-mono text-[0.75rem] leading-[1.35] font-medium whitespace-nowrap',
  {
    variants: {
      variant: {
        default: 'border-transparent bg-primary text-primary-foreground',
        secondary: 'border-transparent bg-muted text-foreground',
        outline: 'border-border text-foreground',
        destructive: 'border-transparent bg-destructive text-destructive-foreground',
      },
    },
    defaultVariants: { variant: 'default' },
  },
)

export interface BadgeProps extends React.ComponentProps<'span'>, VariantProps<typeof badgeVariants> {}

export function Badge({ className, variant, ...props }: BadgeProps) {
  return <span className={cn(badgeVariants({ variant }), className)} {...props} />
}

export type StampTone = 'flash' | 'bulletin' | 'routine' | 'ok' | 'held'

const STAMP_TONE: Record<StampTone, string> = {
  flash: 'text-destructive',
  bulletin: 'text-bulletin',
  routine: 'text-muted-foreground',
  ok: 'text-ok',
  held: 'text-held',
}

/**
 * A tilted, outlined priority stamp (FLASH, BULLETIN, ROUTINE, …): the Release Wire's status
 * vocabulary. The text is the signal; colour only repeats it.
 */
export function Stamp({ tone, className, ...props }: React.ComponentProps<'span'> & { tone: StampTone }) {
  return (
    <span
      className={cn(
        'inline-block -rotate-3 rounded-[2px] border-[1.5px] border-current px-1.5 py-px font-mono text-[0.72rem] leading-[1.3] font-bold tracking-[0.1em] whitespace-nowrap uppercase',
        STAMP_TONE[tone],
        className,
      )}
      {...props}
    />
  )
}
