import { useState, useEffect } from 'react'
import { useForm } from 'react-hook-form'
import { apikeysApi } from '../../api/apikeys'
import { Modal } from '../../components/Modal/Modal'
import { RoleGuard } from '../../components/RoleGuard'
import type { ApiKey, CreateKeyResponse } from '../../types/apikey'
import styles from './ApiKeysPage.module.css'

// ── Setup guide shown once after key creation ─────────────────────────────────

function CopyButton({ text, label = 'Copy' }: { text: string; label?: string }) {
  const [copied, setCopied] = useState(false)
  const copy = () => {
    navigator.clipboard.writeText(text)
    setCopied(true)
    setTimeout(() => setCopied(false), 2000)
  }
  return (
    <button className={styles.copyBtn} onClick={copy}>
      {copied ? '✓ Copied' : label}
    </button>
  )
}

function CodeBlock({ code, lang }: { code: string; lang?: string }) {
  return (
    <div className={styles.codeBlock}>
      {lang && <span className={styles.codeLang}>{lang}</span>}
      <CopyButton text={code} />
      <pre><code>{code}</code></pre>
    </div>
  )
}

function SetupGuide({ apiKey, onDismiss }: { apiKey: string; onDismiss: () => void }) {
  const [tab, setTab] = useState<'python' | 'go'>('python')

  const envBlock = `export SYSTEMSCALE_API_KEY="${apiKey}"`

  const pythonInstall = `pip install systemscale`
  const pythonQuickstart = `import systemscale

client = systemscale.init(
    api_key="${apiKey}",
    project="my-fleet",   # replace with your project name
    device="my-device",   # replace with this device's name
)

# Stream telemetry at up to 50 Hz
client.log({"sensors/temp": 85.2, "nav/altitude": 102.3})

# Send an alert to all operators
client.alert("Low battery", level="warning")

# Request human-in-the-loop approval
response = client.request_assistance("Obstacle detected", timeout=60)
if response.approved:
    print("Proceed:", response.instruction)`

  const goInstall = `go get github.com/systemscale/sdk/go/systemscale`
  const goQuickstart = `package main

import (
    "time"
    "github.com/systemscale/sdk/go/systemscale"
)

func main() {
    client, _ := systemscale.Init(systemscale.Config{
        APIKey:  "${apiKey}",
        Project: "my-fleet",  // replace with your project name
        Device:  "my-device", // replace with this device's name
    })

    // Stream telemetry
    client.Log(map[string]any{
        "sensors/temp":  85.2,
        "nav/altitude":  102.3,
    })

    // Send an alert to all operators
    client.Alert("Low battery",
        systemscale.WithAlertLevel("warning"))

    time.Sleep(time.Second) // let background sender flush
}`

  return (
    <div className={styles.setupGuide}>
      {/* Key reveal */}
      <div className={styles.keyReveal}>
        <span className={styles.keyRevealLabel}>Your API key — copy it now, it won't be shown again.</span>
        <div className={styles.newKey}>
          <code>{apiKey}</code>
          <CopyButton text={apiKey} label="Copy key" />
        </div>
      </div>

      <hr className={styles.divider} />

      {/* Step 1 */}
      <div className={styles.step}>
        <div className={styles.stepHead}>
          <span className={styles.stepNum}>1</span>
          <span className={styles.stepTitle}>Set your environment variable</span>
        </div>
        <p className={styles.stepDesc}>
          Add this to your shell profile or <code>.env</code> file so every SDK call picks up your key automatically.
        </p>
        <CodeBlock code={envBlock} lang="bash" />
      </div>

      {/* Step 2 */}
      <div className={styles.step}>
        <div className={styles.stepHead}>
          <span className={styles.stepNum}>2</span>
          <span className={styles.stepTitle}>Install the SDK</span>
        </div>

        <div className={styles.tabs}>
          <button
            className={`${styles.tab} ${tab === 'python' ? styles.tabActive : ''}`}
            onClick={() => setTab('python')}
          >Python</button>
          <button
            className={`${styles.tab} ${tab === 'go' ? styles.tabActive : ''}`}
            onClick={() => setTab('go')}
          >Go</button>
        </div>

        {tab === 'python' && <CodeBlock code={pythonInstall} lang="bash" />}
        {tab === 'go'     && <CodeBlock code={goInstall}     lang="bash" />}
      </div>

      {/* Step 3 */}
      <div className={styles.step}>
        <div className={styles.stepHead}>
          <span className={styles.stepNum}>3</span>
          <span className={styles.stepTitle}>Quick start</span>
        </div>
        <p className={styles.stepDesc}>
          Replace <code>my-fleet</code> with your project name and <code>my-device</code> with this device's name, then run.
        </p>
        {tab === 'python' && <CodeBlock code={pythonQuickstart} lang="python" />}
        {tab === 'go'     && <CodeBlock code={goQuickstart}     lang="go"     />}
      </div>

      <button className={styles.dismissBtn} onClick={onDismiss}>
        Got it — dismiss
      </button>
    </div>
  )
}

// ── Create key modal ──────────────────────────────────────────────────────────

function CreateKeyModal({ onClose, onCreated }: { onClose: () => void; onCreated: (k: CreateKeyResponse) => void }) {
  const { register, handleSubmit } = useForm<{ name: string; scopes: string }>()
  const [loading, setLoading] = useState(false)
  const [error,   setError]   = useState<string | null>(null)

  const submit = async (data: { name: string; scopes: string }) => {
    setLoading(true)
    setError(null)
    try {
      const scopes = data.scopes.split(',').map((s) => s.trim()).filter(Boolean)
      const key = await apikeysApi.createKey({ name: data.name, scopes })
      onCreated(key)
      onClose()
    } catch (e) {
      setError(String(e))
    } finally {
      setLoading(false)
    }
  }

  return (
    <Modal open onClose={onClose} title="Create API Key">
      <form onSubmit={handleSubmit(submit)} style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
        <div>
          <label style={{ fontSize: 12, color: 'var(--color-text-dim)' }}>Name</label>
          <input {...register('name')} placeholder="my-integration" required />
        </div>
        <div>
          <label style={{ fontSize: 12, color: 'var(--color-text-dim)' }}>Scopes (comma-separated)</label>
          <input {...register('scopes')} placeholder="operator, viewer" />
        </div>
        {error && <div className="error-msg">{error}</div>}
        <button type="submit" className="primary" disabled={loading}>
          {loading ? 'Creating…' : 'Create Key'}
        </button>
      </form>
    </Modal>
  )
}

// ── Page ──────────────────────────────────────────────────────────────────────

export function ApiKeysPage() {
  const [keys,       setKeys]       = useState<ApiKey[]>([])
  const [loading,    setLoading]    = useState(true)
  const [error,      setError]      = useState<string | null>(null)
  const [showCreate, setShowCreate] = useState(false)
  const [newKey,     setNewKey]     = useState<CreateKeyResponse | null>(null)

  const load = async () => {
    try {
      const res = await apikeysApi.listKeys()
      setKeys(res.keys ?? [])
      setError(null)
    } catch (e) { setError(String(e)) }
    finally { setLoading(false) }
  }

  useEffect(() => { load() }, [])

  const revoke = async (id: string) => {
    if (!confirm('Revoke this API key? This cannot be undone.')) return
    await apikeysApi.revokeKey(id)
    await load()
  }

  return (
    <RoleGuard role="admin" fallback={<div className="page-content"><div className="error-msg">Admin role required to manage API keys.</div></div>}>
      <div className="page-content">
        <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
          <h2>API Keys</h2>
          <button className="primary" onClick={() => setShowCreate(true)}>+ Create Key</button>
        </div>

        {/* Setup guide — shown once after key creation */}
        {newKey && (
          <SetupGuide apiKey={newKey.api_key} onDismiss={() => setNewKey(null)} />
        )}

        {loading && <div className="spinner" />}
        {error   && <div className="error-msg">{error}</div>}

        <div className="card" style={{ overflow: 'auto' }}>
          <table>
            <thead>
              <tr>
                <th>Name</th>
                <th>Prefix</th>
                <th>Scopes</th>
                <th>Created</th>
                <th>Last Used</th>
                <th>Status</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              {keys.map((k) => (
                <tr key={k.id}>
                  <td>{k.name}</td>
                  <td><code style={{ fontFamily: 'var(--font-mono)', fontSize: 11 }}>{k.key_prefix}…</code></td>
                  <td>{k.scopes.join(', ') || '—'}</td>
                  <td style={{ fontSize: 11 }}>{new Date(k.created_at).toLocaleDateString()}</td>
                  <td style={{ fontSize: 11 }}>{k.last_used_at ? new Date(k.last_used_at).toLocaleDateString() : 'Never'}</td>
                  <td>
                    {k.revoked_at
                      ? <span className="badge red">REVOKED</span>
                      : <span className="badge green">ACTIVE</span>
                    }
                  </td>
                  <td>
                    {!k.revoked_at && (
                      <button className="danger" style={{ fontSize: 11, padding: '3px 8px' }} onClick={() => revoke(k.id)}>
                        Revoke
                      </button>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>

        {showCreate && (
          <CreateKeyModal
            onClose={() => setShowCreate(false)}
            onCreated={(k) => { setNewKey(k); load() }}
          />
        )}
      </div>
    </RoleGuard>
  )
}
