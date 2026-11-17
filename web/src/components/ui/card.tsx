import * as React from 'react'
import { cn } from '@/lib/utils'

/** A square panel with a hairline: no radius, no shadow (only paper casts shadows). */
export function Card({ className, ...props }: React.ComponentProps<'div'>) {
  return <div className={cn('border border-border bg-card', className)} {...props} />
}

export function CardHeader({ className, ...props }: React.ComponentProps<'div'>) {
  return <div className={cn('flex flex-col gap-1.5 p-5', className)} {...props} />
}

export function CardTitle({ className, ...props }: React.ComponentProps<'h3'>) {
  return <h3 className={cn('wire-subtitle text-lg', className)} {...props} />
}

export function CardDescription({ className, ...props }: React.ComponentProps<'p'>) {
  return <p className={cn('text-sm text-muted-foreground', className)} {...props} />
}

export function CardContent({ className, ...props }: React.ComponentProps<'div'>) {
  return <div className={cn('p-5 pt-0', className)} {...props} />
}
