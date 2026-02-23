import { useEffect, useState } from 'react'
import { useLocation } from 'react-router-dom'
import { wsManager } from '../../ws/WsManager'
import type { WsStatus } from '../../ws/WsManager'
import styles from './TopBar.module.css'

const PAGE_TITLES: Record<string, string> = {
  '/':                    'Projects',
  '/team':                'Team',
  '/settings/api-keys':   'API Keys',
}

function pageTitle(pathname: string): string {
  if (pathname.startsWith('/projects/')) {
    const name = pathname.split('/')[2]
    return name ? decodeURIComponent(name) : 'Project'
  }
  return PAGE_TITLES[pathname] ?? 'Dashboard'
}

function WsIndicator() {
  const [status, setStatus] = useState<WsStatus>(wsManager.status)

  useEffect(() => wsManager.onStatusChange(setStatus), [])

  const label =
    status === 'connected'    ? 'Live'        :
    status === 'connecting'   ? 'Connecting…' :
                                'Disconnected'

  return (
    <div className={`${styles.wsStatus} ${styles[status]}`} title={`WebSocket: ${label}`}>
      <span className={styles.dot} />
      <span className={styles.wsLabel}>{label}</span>
    </div>
  )
}

export function TopBar() {
  const location = useLocation()

  return (
    <header className={styles.topbar}>
      <span className={styles.title}>{pageTitle(location.pathname)}</span>
      <WsIndicator />
    </header>
  )
}
