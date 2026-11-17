import * as React from 'react'
import { Slot } from '@radix-ui/react-slot'
import { cva, type VariantProps } from 'class-variance-authority'
import { cn } from '@/lib/utils'

/** Release Wire buttons: square slips (2px), gold for the one primary action, line for the rest. */
const buttonVariants = cva(
  'inline-flex items-center justify-center gap-2 rounded-[2px] text-sm font-[750] [font-stretch:82%] whitespace-nowrap transition-[background-color,color,box-shadow,transform] duration-150 ease-[cubic-bezier(.2,.8,.2,1)] focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring disabled:pointer-events-none disabled:opacity-50',
  {
    variants: {
      variant: {
        default: 'bg-primary text-primary-foreground hover:bg-canary hover:-translate-y-px',
        outline: 'bg-transparent text-foreground shadow-[inset_0_0_0_1.5px_var(--border)] hover:text-gold-ink hover:shadow-[inset_0_0_0_1.5px_var(--gold-ink)]',
        ghost: 'text-foreground hover:bg-muted',
        destructive: 'bg-destructive text-destructive-foreground hover:opacity-90',
      },
      size: { default: 'h-9 px-4 py-2', sm: 'h-8 px-3', lg: 'h-10 px-6' },
    },
    defaultVariants: { variant: 'default', size: 'default' },
  },
)

export interface ButtonProps extends React.ComponentProps<'button'>, VariantProps<typeof buttonVariants> {
  asChild?: boolean
}

export function Button({ className, variant, size, asChild = false, ...props }: ButtonProps) {
  const Comp = asChild ? Slot : 'button'
  return <Comp className={cn(buttonVariants({ variant, size }), className)} {...props} />
}
