import { useEffect, useState } from 'react'
import { membersApi } from '../../api/members'
import { fleetApi } from '../../api/fleet'
import { Modal } from '../../components/Modal/Modal'
import type { OrgMember } from '../../types/member'
import type { Vehicle, Project } from '../../types/vehicle'
import styles from './TeamPage.module.css'

interface ProjectDevices {
  project: Project
  devices: Vehicle[]
}

export function TeamPage() {
  const [members, setMembers] = useState<OrgMember[]>([])
  const [loading, setLoading] = useState(true)
  const [projectDevices, setProjectDevices] = useState<ProjectDevices[]>([])

  // Invite modal
  const [showInvite, setShowInvite] = useState(false)
  const [invEmail, setInvEmail] = useState('')
  const [invRole, setInvRole] = useState('viewer')
  const [invDevices, setInvDevices] = useState<Set<string>>(new Set())
  const [inviting, setInviting] = useState(false)

  // Edit modal
  const [editMember, setEditMember] = useState<OrgMember | null>(null)
  const [editRole, setEditRole] = useState('')
  const [editDevices, setEditDevices] = useState<Set<string>>(new Set())
  const [saving, setSaving] = useState(false)

  const loadAll = async () => {
    try {
      const [memberRes, projectRes] = await Promise.all([
        membersApi.listMembers(),
        fleetApi.listProjects(),
      ])
      setMembers(memberRes.members ?? [])

      const pd = await Promise.all(
        (projectRes.projects ?? []).map(async (p) => {
          try {
            const { devices } = await fleetApi.listProjectDevices(p.name)
            return { project: p, devices: devices ?? [] }
          } catch {
            return { project: p, devices: [] }
          }
        }),
      )
      setProjectDevices(pd)
    } catch {
      // silently handle
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => { loadAll() }, [])

  const allDevices = projectDevices.flatMap((pd) => pd.devices)
  const deviceNameMap = new Map(allDevices.map((d) => [d.id, d.display_name]))

  const toggleDevice = (set: Set<string>, id: string) => {
    const next = new Set(set)
    if (next.has(id)) next.delete(id)
    else next.add(id)
    return next
  }

  const handleInvite = async () => {
    if (!invEmail.trim()) return
    setInviting(true)
    try {
      await membersApi.inviteMember({
        email: invEmail.trim(),
        role: invRole,
        device_ids: [...invDevices],
      })
      setShowInvite(false)
      setInvEmail('')
      setInvRole('viewer')
      setInvDevices(new Set())
      await loadAll()
    } catch {
      // handle error
    } finally {
      setInviting(false)
    }
  }

  const openEdit = (m: OrgMember) => {
    setEditMember(m)
    setEditRole(m.role)
    setEditDevices(new Set(m.device_ids ?? []))
  }

  const handleSaveEdit = async () => {
    if (!editMember) return
    setSaving(true)
    try {
      await membersApi.updateMemberAccess(editMember.id, {
        role: editRole,
        device_ids: [...editDevices],
      })
      setEditMember(null)
      await loadAll()
    } catch {
      // handle error
    } finally {
      setSaving(false)
    }
  }

  const handleRemove = async (id: string) => {
    if (!confirm('Remove this member from the organization?')) return
    try {
      await membersApi.removeMember(id)
      await loadAll()
    } catch {
      // handle error
    }
  }

  const initials = (email: string) =>
    email.split('@')[0].slice(0, 2).toUpperCase()

  const DevicePicker = ({
    selected,
    onToggle,
  }: {
    selected: Set<string>
    onToggle: (id: string) => void
  }) => (
    <div className={styles.devicePicker}>
      {projectDevices.length === 0 ? (
        <div style={{ padding: 12, fontSize: 12, color: 'var(--color-text-dim)' }}>
          No devices available.
        </div>
      ) : (
        projectDevices.map((pd) => (
          <div key={pd.project.id}>
            <div className={styles.projectGroup}>{pd.project.display_name || pd.project.name}</div>
            {pd.devices.map((d) => (
              <label key={d.id} className={styles.deviceCheck}>
                <input
                  type="checkbox"
                  checked={selected.has(d.id)}
                  onChange={() => onToggle(d.id)}
                />
                {d.display_name}
              </label>
            ))}
          </div>
        ))
      )}
    </div>
  )

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
        <h2>Team</h2>
        <button className="primary" onClick={() => setShowInvite(true)}>
          + Invite Member
        </button>
      </div>

      {members.length === 0 ? (
        <div className={styles.empty}>No team members yet.</div>
      ) : (
        <div className="card" style={{ padding: 0, overflow: 'hidden' }}>
          {members.map((m) => (
            <div key={m.id} className={styles.memberRow}>
              <div className={styles.avatar}>{initials(m.email)}</div>
              <div className={styles.memberInfo}>
                <div className={styles.memberEmail}>{m.email}</div>
                {m.name && m.name.trim() && (
                  <div className={styles.memberName}>{m.name}</div>
                )}
                {m.device_ids && m.device_ids.length > 0 && (
                  <div className={styles.memberDevices}>
                    {m.device_ids.map((id) => deviceNameMap.get(id) ?? id.slice(0, 8)).join(', ')}
                  </div>
                )}
              </div>
              <span className={`badge ${m.role === 'admin' ? 'red' : m.role === 'operator' ? 'amber' : 'blue'}`}>
                {m.role || 'viewer'}
              </span>
              <div className={styles.memberActions}>
                <button onClick={() => openEdit(m)}>Edit</button>
                <button className="danger" onClick={() => handleRemove(m.id)}>Remove</button>
              </div>
            </div>
          ))}
        </div>
      )}

      {/* ── Invite modal ─────────────────────────────────────────── */}
      <Modal open={showInvite} onClose={() => setShowInvite(false)} title="Invite Member">
        <div className={styles.formRow}>
          <label>Email</label>
          <input
            type="email"
            placeholder="user@example.com"
            value={invEmail}
            onChange={(e) => setInvEmail(e.target.value)}
          />
        </div>
        <div className={styles.formRow}>
          <label>Role</label>
          <select value={invRole} onChange={(e) => setInvRole(e.target.value)}>
            <option value="viewer">Viewer (read-only)</option>
            <option value="operator">Operator (can send commands)</option>
          </select>
        </div>
        <div className={styles.formRow}>
          <label>Device Access</label>
          <DevicePicker
            selected={invDevices}
            onToggle={(id) => setInvDevices(toggleDevice(invDevices, id))}
          />
        </div>
        <div className={styles.formActions}>
          <button onClick={() => setShowInvite(false)}>Cancel</button>
          <button className="primary" onClick={handleInvite} disabled={inviting || !invEmail.trim()}>
            {inviting ? 'Inviting...' : 'Send Invite'}
          </button>
        </div>
      </Modal>

      {/* ── Edit modal ───────────────────────────────────────────── */}
      <Modal
        open={!!editMember}
        onClose={() => setEditMember(null)}
        title={`Edit Access — ${editMember?.email ?? ''}`}
      >
        <div className={styles.formRow}>
          <label>Role</label>
          <select value={editRole} onChange={(e) => setEditRole(e.target.value)}>
            <option value="viewer">Viewer</option>
            <option value="operator">Operator</option>
            <option value="admin">Admin</option>
          </select>
        </div>
        <div className={styles.formRow}>
          <label>Device Access</label>
          <DevicePicker
            selected={editDevices}
            onToggle={(id) => setEditDevices(toggleDevice(editDevices, id))}
          />
        </div>
        <div className={styles.formActions}>
          <button onClick={() => setEditMember(null)}>Cancel</button>
          <button className="primary" onClick={handleSaveEdit} disabled={saving}>
            {saving ? 'Saving...' : 'Save Changes'}
          </button>
        </div>
      </Modal>
    </div>
  )
}
