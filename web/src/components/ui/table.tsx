import * as React from 'react'
import { cn } from '@/lib/utils'

/** A ledger: a heavy top rule, mono column heads in burnt gold, hairline rows. */
export function Table({ className, ...props }: React.ComponentProps<'table'>) {
  return (
    <div className="w-full overflow-auto">
      <table className={cn('w-full caption-bottom text-sm tabular-nums', className)} {...props} />
    </div>
  )
}

export function TableHeader({ className, ...props }: React.ComponentProps<'thead'>) {
  return <thead className={cn('[&_tr]:border-b-2 [&_tr]:border-foreground [&_tr:hover]:bg-transparent', className)} {...props} />
}

export function TableBody({ className, ...props }: React.ComponentProps<'tbody'>) {
  return <tbody className={cn('[&_tr:last-child]:border-0', className)} {...props} />
}

export function TableRow({ className, ...props }: React.ComponentProps<'tr'>) {
  return <tr className={cn('border-b border-border transition-colors hover:bg-primary/10', className)} {...props} />
}

export function TableHead({ className, ...props }: React.ComponentProps<'th'>) {
  return <th className={cn('h-9 px-3 text-left align-middle whitespace-nowrap', className)} {...props} />
}

export function TableCell({ className, ...props }: React.ComponentProps<'td'>) {
  return <td className={cn('px-3 py-2.5 align-middle', className)} {...props} />
}
