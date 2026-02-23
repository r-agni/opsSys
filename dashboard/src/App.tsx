import { useEffect, useState } from 'react'
import { BrowserRouter, Routes, Route, Navigate, Outlet } from 'react-router-dom'
import { useAuthStore } from './store/authStore'
import { NavSidebar } from './components/NavSidebar/NavSidebar'
import { TopBar }     from './components/TopBar/TopBar'
import { LoginPage }         from './pages/Login/LoginPage'
import { AuthCallbackPage }  from './pages/AuthCallback/AuthCallbackPage'
import { HomePage }          from './pages/Home/HomePage'
import { ProjectPage }       from './pages/Project/ProjectPage'
import { TeamPage }          from './pages/Team/TeamPage'
import { ApiKeysPage }       from './pages/ApiKeys/ApiKeysPage'
import { RoleGuard }         from './components/RoleGuard'

function AuthGuard() {
  const { token, loadExisting } = useAuthStore()
  const [checked, setChecked] = useState(false)

  useEffect(() => {
    loadExisting().finally(() => setChecked(true))
  }, []) // eslint-disable-line react-hooks/exhaustive-deps

  if (!checked) {
    return (
      <div style={{ display: 'flex', height: '100vh', alignItems: 'center', justifyContent: 'center' }}>
        <span className="spinner" />
      </div>
    )
  }

  if (!token) return <Navigate to="/login" replace />

  return (
    <div style={{ display: 'flex', height: '100vh', overflow: 'hidden' }}>
      <NavSidebar />
      <div style={{ flex: 1, display: 'flex', flexDirection: 'column', overflow: 'hidden' }}>
        <TopBar />
        <main style={{ flex: 1, display: 'flex', flexDirection: 'column', overflowY: 'auto', minHeight: 0 }}>
          <Outlet />
        </main>
      </div>
    </div>
  )
}

function AdminGuard() {
  return (
    <RoleGuard role="admin" fallback={<Navigate to="/" replace />}>
      <Outlet />
    </RoleGuard>
  )
}

export default function App() {
  return (
    <BrowserRouter>
      <Routes>
        <Route path="/login"         element={<LoginPage />} />
        <Route path="/auth/callback" element={<AuthCallbackPage />} />

        <Route element={<AuthGuard />}>
          <Route path="/"                   element={<HomePage />} />
          <Route path="/projects/:name"     element={<ProjectPage />} />

          <Route element={<AdminGuard />}>
            <Route path="/team"               element={<TeamPage />} />
            <Route path="/settings/api-keys"  element={<ApiKeysPage />} />
          </Route>
        </Route>

        <Route path="*" element={<Navigate to="/" replace />} />
      </Routes>
    </BrowserRouter>
  )
}
