import type { ReactNode } from 'react'
import { useAuthStore } from '../store/authStore'
import type { AuthClaims } from '../store/authStore'

interface Props {
  role: AuthClaims['role']  // minimum required role
  children: ReactNode
  fallback?: ReactNode
}

const ROLE_RANK: Record<AuthClaims['role'], number> = {
  viewer: 0, operator: 1, admin: 2,
}

export function RoleGuard({ role, children, fallback = null }: Props) {
  const userRole = useAuthStore((s) => s.claims?.role ?? 'viewer')
  if (ROLE_RANK[userRole] < ROLE_RANK[role]) return <>{fallback}</>
  return <>{children}</>
}
