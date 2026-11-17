/** The Angelia mark: the A as a rising peak with a herald's wing of three gold feathers and a spark. */
export function Glyph({ className, stroke = 'currentColor' }: { className?: string; stroke?: string }) {
  return (
    <svg viewBox="80 10 360 420" aria-hidden="true" focusable="false" className={className}>
      <path d="M112 412 L256 108 L400 412" fill="none" stroke={stroke} strokeWidth="44" strokeLinejoin="round" strokeLinecap="round" />
      <path d="M168 316 C246 318 330 298 418 236 C374 306 286 338 180 342 Z" fill="#F59E0B" />
      <path d="M184 272 C254 270 330 246 404 186 C366 254 292 290 196 298 Z" fill="#FBBF24" />
      <path d="M202 228 C258 222 318 196 380 140 C352 206 296 240 214 254 Z" fill="#FDE68A" />
      <path d="M256 20 L266 48 L294 58 L266 68 L256 96 L246 68 L218 58 L246 48 Z" fill="#F59E0B" />
    </svg>
  )
}

/** The gold Baudot tape: the Release Wire's one recurring rule. */
export function Tape({ className = '' }: { className?: string }) {
  return <div aria-hidden="true" className={`baudot ${className}`} />
}
