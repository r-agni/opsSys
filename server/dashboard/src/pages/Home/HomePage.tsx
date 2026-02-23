import { useEffect, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { useAuthStore } from '../../store/authStore'
import { fleetApi } from '../../api/fleet'
import { Modal } from '../../components/Modal/Modal'
import type { Project, Vehicle } from '../../types/vehicle'
import styles from './HomePage.module.css'

export function HomePage() {
  const navigate = useNavigate()
  const isAdmin = useAuthStore((s) => s.isAdmin)
  const vehicleSet = useAuthStore((s) => s.claims?.vehicle_set ?? [])

  const [projects, setProjects] = useState<(Project & { deviceCount: number })[]>([])
  const [loading, setLoading] = useState(true)
  const [showCreate, setShowCreate] = useState(false)
  const [newName, setNewName] = useState('')
  const [newDisplayName, setNewDisplayName] = useState('')
  const [creating, setCreating] = useState(false)
  const [createError, setCreateError] = useState('')

  const load = async () => {
    try {
      const { projects: pList } = await fleetApi.listProjects()
      const withCounts = await Promise.all(
        pList.map(async (p) => {
          try {
            const { devices } = await fleetApi.listProjectDevices(p.name)
            return { ...p, deviceCount: devices?.length ?? 0, _devices: devices ?? [] }
          } catch {
            return { ...p, deviceCount: 0, _devices: [] as Vehicle[] }
          }
        }),
      )

      if (isAdmin()) {
        setProjects(withCounts)
      } else {
        const vsSet = new Set(vehicleSet)
        setProjects(
          withCounts.filter((p) =>
            (p as typeof p & { _devices: Vehicle[] })._devices.some((d: Vehicle) => vsSet.has(d.id)),
          ),
        )
      }
    } catch {
      // silently handle
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => { load() }, [])

  const handleCreate = async () => {
    if (!newName.trim()) return
    setCreating(true)
    setCreateError('')
    try {
      await fleetApi.createProject(newName.trim(), newDisplayName.trim() || newName.trim())
      setShowCreate(false)
      setNewName('')
      setNewDisplayName('')
      await load()
    } catch (err) {
      setCreateError(err instanceof Error ? err.message : 'Failed to create project')
    } finally {
      setCreating(false)
    }
  }

  if (loading) {
    return (
      <div className={styles.root}>
        <div style={{ display: 'flex', justifyContent: 'center', padding: 60 }}>
          <div className="spinner" />
        </div>
      </div>
    )
  }

  return (
    <div className={styles.root}>
      <div className={styles.header}>
        <h2>Projects</h2>
        {isAdmin() && (
          <button className="primary" onClick={() => setShowCreate(true)}>
            + New Project
          </button>
        )}
      </div>

      {projects.length === 0 ? (
        <div className={styles.empty}>
          {isAdmin()
            ? 'No projects yet. Create your first project to get started.'
            : 'No projects assigned to your account.'}
        </div>
      ) : (
        <div className={styles.grid}>
          {projects.map((p) => (
            <div
              key={p.id}
              className={styles.card}
              onClick={() => navigate(`/projects/${p.name}`)}
            >
              <div className={styles.cardName}>{p.display_name || p.name}</div>
              <div className={styles.cardMeta}>
                <span>{p.deviceCount} device{p.deviceCount !== 1 ? 's' : ''}</span>
                <span>{new Date(p.created_at).toLocaleDateString()}</span>
              </div>
            </div>
          ))}
        </div>
      )}

      <Modal open={showCreate} onClose={() => { setShowCreate(false); setCreateError('') }} title="Create Project">
        {createError && <div className={styles.error}>{createError}</div>}
        <div className={styles.formRow}>
          <label>Project Name (slug)</label>
          <input
            placeholder="my-fleet"
            value={newName}
            onChange={(e) => setNewName(e.target.value)}
            onKeyDown={(e) => e.key === 'Enter' && handleCreate()}
          />
        </div>
        <div className={styles.formRow}>
          <label>Display Name (optional)</label>
          <input
            placeholder="My Fleet"
            value={newDisplayName}
            onChange={(e) => setNewDisplayName(e.target.value)}
            onKeyDown={(e) => e.key === 'Enter' && handleCreate()}
          />
        </div>
        <div className={styles.formActions}>
          <button onClick={() => setShowCreate(false)}>Cancel</button>
          <button className="primary" onClick={handleCreate} disabled={creating || !newName.trim()}>
            {creating ? 'Creating...' : 'Create'}
          </button>
        </div>
      </Modal>
    </div>
  )
}
