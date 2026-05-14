interface Props {
  size?: number
  className?: string
  glow?: boolean
}

export default function Bolt({ size = 20, className = '', glow = true }: Props) {
  const glowStyle = glow
    ? { filter: 'drop-shadow(0 0 6px rgba(20, 184, 166, 0.55))' }
    : undefined
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      xmlns="http://www.w3.org/2000/svg"
      className={className}
      style={glowStyle}
      aria-hidden="true"
    >
      <defs>
        <linearGradient id="vajra-bolt-gradient" x1="0" y1="0" x2="0" y2="1">
          <stop offset="0%" stopColor="#5eead4" />
          <stop offset="55%" stopColor="#14b8a6" />
          <stop offset="100%" stopColor="#0d9488" />
        </linearGradient>
      </defs>
      <path
        d="M13.6 1.4 4.2 13.1c-.5.6-.07 1.5.7 1.5h5.0l-1.9 7.4c-.2.9.9 1.6 1.6 1.0L19.8 10.9c.5-.6.07-1.5-.7-1.5h-5.0l1.9-7.4c.2-.9-.9-1.6-1.6-1.0Z"
        fill="url(#vajra-bolt-gradient)"
        stroke="#14b8a6"
        strokeWidth="0.6"
        strokeLinejoin="round"
      />
    </svg>
  )
}
