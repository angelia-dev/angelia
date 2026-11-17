import type { StampTone } from '@/components/ui/badge'

/** The release's priority, from its bump: major is FLASH, minor BULLETIN, the rest ROUTINE. */
export function bumpTone(bump: string | null | undefined): { tone: StampTone; label: string } {
  if (bump === 'major') return { tone: 'flash', label: 'Flash' }
  if (bump === 'minor') return { tone: 'bulletin', label: 'Bulletin' }
  return { tone: 'routine', label: 'Routine' }
}
