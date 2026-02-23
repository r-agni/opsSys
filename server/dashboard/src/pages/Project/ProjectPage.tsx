import { useEffect, useRef, useState } from 'react'
import { useNavigate, useParams } from 'react-router-dom'
import { useAuthStore } from '../../store/authStore'
import { useTelemetryStore } from '../../store/telemetryStore'
import { useAlertStore } from '../../store/alertStore'
import { fleetApi } from '../../api/fleet'
import { wsManager } from '../../ws/WsManager'
import { Modal } from '../../components/Modal/Modal'
import type { Vehicle } from '../../types/vehicle'
import type { TelemetryFrame, AlertFrame } from '../../types/telemetry'
import styles from './ProjectPage.module.css'

const MAX_FEED_ROWS = 500

export function ProjectPage() {
  const { name } = useParams<{ name: string }>()
  const navigate = useNavigate()
  const isAdmin = useAuthStore((s) => s.isAdmin)

  const [devices, setDevices] = useState<Vehicle[]>([])
  const [loading, setLoading] = useState(true)
  const [tab, setTab] = useState<'data' | 'alerts'>('data')

  // Add device modal
  const [showAdd, setShowAdd] = useState(false)
  const [devName, setDevName] = useState('')
  const [devType, setDevType] = useState('')
  const [devRegion, setDevRegion] = useState('')
  const [adding, setAdding] = useState(false)

  // Live telemetry feed
  const [feedRows, setFeedRows] = useState<TelemetryFrame[]>([])
  const feedRef = useRef<TelemetryFrame[]>([])
  const rafRef = useRef(0)
  const genRef = useRef(0)

  const [error, setError] = useState('')

  const loadDevices = async () => {
    if (!name) return
    try {
      const { devices: d } = await fleetApi.listProjectDevices(name)
      setDevices(d ?? [])
    } catch (e) {
      console.error('Failed to load devices', e)
      setError('Failed to load devices. Check console for details.')
      setDevices([])
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => { loadDevices() }, [name])

  // Subscribe to all project devices for live telemetry
  useEffect(() => {
    if (devices.length === 0) return
    const ids = devices.map((d) => d.id)
    ids.forEach((id) => wsManager.subscribe(id))

    const tick = () => {
      const store = useTelemetryStore.getState()
      if (store.generation !== genRef.current) {
        genRef.current = store.generation
        const allFrames: TelemetryFrame[] = []
        for (const id of ids) {
          const buf = store.getBuffer(id)
          if (buf) allFrames.push(...(buf.snapshot() as TelemetryFrame[]))
        }
        allFrames.sort((a, b) => b.ts - a.ts)
        const trimmed = allFrames.slice(0, MAX_FEED_ROWS)
        feedRef.current = trimmed
        setFeedRows(trimmed)
      }
      rafRef.current = requestAnimationFrame(tick)
    }
    rafRef.current = requestAnimationFrame(tick)

    return () => {
      cancelAnimationFrame(rafRef.current)
      ids.forEach((id) => wsManager.unsubscribe(id))
    }
  }, [devices])

  const deviceIds = new Set(devices.map((d) => d.id))
  const alerts = useAlertStore((s) =>
    s.alerts.filter((a: AlertFrame) => deviceIds.has(a.device)),
  )

  const deviceNameMap = new Map(devices.map((d) => [d.id, d.display_name]))

  const handleAddDevice = async () => {
    if (!name || !devName.trim()) return
    setAdding(true)
    try {
      await fleetApi.provisionDevice(name, {
        display_name: devName.trim(),
        vehicle_type: devType.trim() || undefined,
        region: devRegion.trim() || undefined,
      })
      setShowAdd(false)
      setDevName('')
      setDevType('')
      setDevRegion('')
      await loadDevices()
    } catch {
      // handle error
    } finally {
      setAdding(false)
    }
  }

  const formatTs = (ns: number) => {
    const d = new Date(ns / 1e6)
    return d.toLocaleTimeString([], { hour12: false, fractionalSecondDigits: 3 } as Intl.DateTimeFormatOptions)
  }

  const levelColor = (level: string) => {
    switch (level) {
      case 'critical': case 'error': return 'red'
      case 'warning': return 'amber'
      default: return 'blue'
    }
  }

  if (loading) {
    return (
      <div className={styles.root}>
        <div style={{ display: 'flex', justifyContent: 'center', alignItems: 'center', flex: 1 }}>
          <div className="spinner" />
        </div>
      </div>
    )
  }

  if (error) {
    return (
      <div className={styles.root}>
        <div style={{ display: 'flex', flexDirection: 'column', justifyContent: 'center', alignItems: 'center', flex: 1, gap: 16, color: 'var(--color-text-dim)' }}>
          <span style={{ fontSize: 14 }}>{error}</span>
          <button onClick={() => { setError(''); setLoading(true); loadDevices() }}>Retry</button>
        </div>
      </div>
    )
  }

  return (
    <div className={styles.root}>
      {/* ── Device sidebar ────────────────────────────────────────── */}
      <aside className={styles.sidebar}>
        <div className={styles.sidebarHeader}>
          <h3>Devices</h3>
          {isAdmin() && (
            <button style={{ fontSize: 12, padding: '4px 10px' }} onClick={() => setShowAdd(true)}>
              + Add
            </button>
          )}
        </div>
        <div className={styles.deviceList}>
          {devices.length === 0 ? (
            <div className={styles.noDevices}>No devices provisioned yet.</div>
          ) : (
            devices.map((d) => (
              <div key={d.id} className={styles.deviceRow}>
                <span className={styles.deviceDot} />
                <span className={styles.deviceName}>{d.display_name}</span>
                <span className={styles.deviceType}>{d.vehicle_type}</span>
              </div>
            ))
          )}
        </div>
      </aside>

      {/* ── Main panel ────────────────────────────────────────────── */}
      <div className={styles.main}>
        <div className={styles.topBar}>
          <button className={styles.backBtn} onClick={() => navigate('/')}>
            &larr; Projects
          </button>
          <span className={styles.projectTitle}>{name}</span>
        </div>

        <div className={styles.tabs}>
          <button
            className={`${styles.tab} ${tab === 'data' ? styles.tabActive : ''}`}
            onClick={() => setTab('data')}
          >
            Data Stream
          </button>
          <button
            className={`${styles.tab} ${tab === 'alerts' ? styles.tabActive : ''}`}
            onClick={() => setTab('alerts')}
          >
            Alerts ({alerts.length})
          </button>
        </div>

        <div className={styles.tabContent}>
          {tab === 'data' && (
            feedRows.length === 0 ? (
              <div className={styles.emptyTab}>
                No telemetry data streaming yet. Connect devices to see live data.
              </div>
            ) : (
              <table className={styles.dataTable}>
                <thead>
                  <tr>
                    <th>Time</th>
                    <th>Device</th>
                    <th>Lat</th>
                    <th>Lon</th>
                    <th>Alt (m)</th>
                    <th>Seq</th>
                  </tr>
                </thead>
                <tbody>
                  {feedRows.map((row, i) => (
                    <tr key={`${row.vehicle_id}-${row.ts}-${i}`}>
                      <td>{formatTs(row.ts)}</td>
                      <td>{deviceNameMap.get(row.vehicle_id) ?? row.vehicle_id.slice(0, 8)}</td>
                      <td>{row.lat.toFixed(6)}</td>
                      <td>{row.lon.toFixed(6)}</td>
                      <td>{row.alt.toFixed(1)}</td>
                      <td>{row.seq}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )
          )}

          {tab === 'alerts' && (
            alerts.length === 0 ? (
              <div className={styles.emptyTab}>No alerts for this project.</div>
            ) : (
              alerts.map((a: AlertFrame, i: number) => (
                <div key={i} className={styles.alertRow}>
                  <span className={`${styles.alertLevel} badge ${levelColor(a.level)}`}>
                    {a.level}
                  </span>
                  <span className={styles.alertDevice}>
                    {deviceNameMap.get(a.device) ?? a.device.slice(0, 8)}
                  </span>
                  <span className={styles.alertMessage}>{a.message}</span>
                </div>
              ))
            )
          )}
        </div>
      </div>

      {/* ── Add device modal ──────────────────────────────────────── */}
      <Modal open={showAdd} onClose={() => setShowAdd(false)} title="Add Device">
        <div className={styles.formRow}>
          <label>Device Name</label>
          <input
            placeholder="drone-001"
            value={devName}
            onChange={(e) => setDevName(e.target.value)}
            onKeyDown={(e) => e.key === 'Enter' && handleAddDevice()}
          />
        </div>
        <div className={styles.formRow}>
          <label>Type (optional)</label>
          <input
            placeholder="drone, rover, camera..."
            value={devType}
            onChange={(e) => setDevType(e.target.value)}
          />
        </div>
        <div className={styles.formRow}>
          <label>Region (optional)</label>
          <input
            placeholder="us-east-1"
            value={devRegion}
            onChange={(e) => setDevRegion(e.target.value)}
          />
        </div>
        <div className={styles.formActions}>
          <button onClick={() => setShowAdd(false)}>Cancel</button>
          <button className="primary" onClick={handleAddDevice} disabled={adding || !devName.trim()}>
            {adding ? 'Adding...' : 'Add Device'}
          </button>
        </div>
      </Modal>
    </div>
  )
}
