import { NavLink, useNavigate } from 'react-router-dom'
import { useAuthStore } from '../../store/authStore'
import styles from './NavSidebar.module.css'

const NAV_ITEMS = [
  {
    to: '/', label: 'Projects', adminOnly: false, icon: (
      <svg width="15" height="15" viewBox="0 0 15 15" fill="none" aria-hidden>
        <rect x="1" y="1" width="5.5" height="5.5" rx="1.2" fill="currentColor" opacity=".9"/>
        <rect x="8.5" y="1" width="5.5" height="5.5" rx="1.2" fill="currentColor" opacity=".9"/>
        <rect x="1" y="8.5" width="5.5" height="5.5" rx="1.2" fill="currentColor" opacity=".9"/>
        <rect x="8.5" y="8.5" width="5.5" height="5.5" rx="1.2" fill="currentColor" opacity=".9"/>
      </svg>
    ),
  },
  {
    to: '/team', label: 'Team', adminOnly: true, icon: (
      <svg width="15" height="15" viewBox="0 0 15 15" fill="none" aria-hidden>
        <circle cx="5.5" cy="4.5" r="2.3" stroke="currentColor" strokeWidth="1.3"/>
        <path d="M1 12.5c0-2.2 2-4 4.5-4s4.5 1.8 4.5 4" stroke="currentColor" strokeWidth="1.3" strokeLinecap="round"/>
        <circle cx="11" cy="5" r="1.6" stroke="currentColor" strokeWidth="1.1"/>
        <path d="M10.5 9c1.5 0 3.5 1 3.5 3" stroke="currentColor" strokeWidth="1.1" strokeLinecap="round"/>
      </svg>
    ),
  },
  {
    to: '/settings/api-keys', label: 'API Keys', adminOnly: true, icon: (
      <svg width="15" height="15" viewBox="0 0 15 15" fill="none" aria-hidden>
        <circle cx="5" cy="7.5" r="3.2" stroke="currentColor" strokeWidth="1.3"/>
        <path d="M7.5 7.5H14M11.5 6V7.5M13.5 7.5V9" stroke="currentColor" strokeWidth="1.3" strokeLinecap="round"/>
      </svg>
    ),
  },
]

function getInitials(email?: string, sub?: string): string {
  const s = email ?? sub ?? '?'
  const name = s.split('@')[0]
  const parts = name.split(/[._\-\s]+/)
  return parts.length >= 2
    ? (parts[0][0] + parts[1][0]).toUpperCase()
    : name.slice(0, 2).toUpperCase()
}

export function NavSidebar() {
  const { claims, logout, isAdmin } = useAuthStore()
  const navigate = useNavigate()
  const admin = isAdmin()

  return (
    <nav className={styles.sidebar}>
      <div className={styles.brand}>
        <svg className={styles.logoMark} width="20" height="20" viewBox="0 0 20 20" fill="none" aria-hidden>
          <rect x="1" y="1" width="8" height="8" rx="1.8" fill="var(--color-accent)"/>
          <rect x="11" y="1" width="8" height="8" rx="1.8" fill="var(--color-accent)" opacity="0.55"/>
          <rect x="1" y="11" width="8" height="8" rx="1.8" fill="var(--color-accent)" opacity="0.55"/>
          <rect x="11" y="11" width="8" height="8" rx="1.8" fill="var(--color-accent)" opacity="0.2"/>
        </svg>
        <span className={styles.logoText}>SystemScale</span>
      </div>

      <ul className={styles.nav}>
        {NAV_ITEMS
          .filter((item) => !item.adminOnly || admin)
          .map(({ to, label, icon }) => (
            <li key={to}>
              <NavLink
                to={to}
                end={to === '/'}
                className={({ isActive }) =>
                  `${styles.link}${isActive ? ` ${styles.active}` : ''}`
                }
              >
                <span className={styles.icon}>{icon}</span>
                {label}
              </NavLink>
            </li>
          ))}
      </ul>

      <div className={styles.footer}>
        <div className={styles.user}>
          <div className={styles.avatar}>
            {getInitials(claims?.email, claims?.sub)}
          </div>
          <div className={styles.userInfo}>
            <div className={styles.email}>{claims?.email ?? claims?.sub ?? 'Operator'}</div>
            <div className={styles.rolePill}>{claims?.role ?? 'viewer'}</div>
          </div>
        </div>
        <button
          className={styles.logoutBtn}
          onClick={() => { logout(); navigate('/login') }}
          title="Sign out"
        >
          <svg width="13" height="13" viewBox="0 0 13 13" fill="none" aria-hidden>
            <path d="M4.5 1.5H2A1.5 1.5 0 0 0 .5 3v7A1.5 1.5 0 0 0 2 11.5h2.5M8.5 9l3-2.5L8.5 4M4.5 6.5H12" stroke="currentColor" strokeWidth="1.3" strokeLinecap="round" strokeLinejoin="round"/>
          </svg>
          Sign out
        </button>
      </div>
    </nav>
  )
}
