import { Navigate, Route, Routes } from 'react-router-dom'
import { AuthProvider, useAuth } from './auth/AuthContext'
import { ToastProvider } from './components/Toast'
import Layout from './components/Layout'
import Landing from './pages/Landing'
import LoginPage from './pages/Login'
import Dashboard from './pages/Dashboard'
import SandboxesPage from './pages/Sandboxes'
import SandboxDetailPage from './pages/SandboxDetail'
import TemplatesPage from './pages/Templates'
import NodesPage from './pages/Nodes'
import ApiKeysPage from './pages/ApiKeys'
import UsagePage from './pages/Usage'
import AdminPage from './pages/Admin'
import MetricsPage from './pages/Metrics'
import WebhooksPage from './pages/Webhooks'
import SnapshotsPage from './pages/Snapshots'
import SettingsPage from './pages/Settings'

function RequireAuth({ children }: { children: React.ReactNode }) {
  const { token } = useAuth()
  if (!token) return <Navigate to="/login" replace />
  return <>{children}</>
}

// RootRoute resolves "/": the marketing landing page for visitors, or a
// bounce into the app for anyone with an active session.
function RootRoute() {
  const { token } = useAuth()
  if (token) return <Navigate to="/sandboxes" replace />
  return <Landing />
}

function AppRoutes() {
  return (
    <Routes>
      <Route path="/" element={<RootRoute />} />
      <Route path="/login" element={<LoginPage />} />
      <Route path="/signup" element={<LoginPage initialMode="register" />} />
      <Route
        element={
          <RequireAuth>
            <Layout />
          </RequireAuth>
        }
      >
        <Route path="dashboard" element={<Dashboard />} />
        <Route path="sandboxes" element={<SandboxesPage />} />
        <Route path="sandboxes/:id" element={<SandboxDetailPage />} />
        <Route path="snapshots" element={<SnapshotsPage />} />
        <Route path="templates" element={<TemplatesPage />} />
        <Route path="nodes" element={<NodesPage />} />
        <Route path="api-keys" element={<ApiKeysPage />} />
        <Route path="webhooks" element={<WebhooksPage />} />
        <Route path="usage" element={<UsagePage />} />
        <Route path="metrics" element={<MetricsPage />} />
        <Route path="settings" element={<SettingsPage />} />
        <Route path="admin" element={<AdminPage />} />
      </Route>
      <Route path="*" element={<Navigate to="/" replace />} />
    </Routes>
  )
}

export default function App() {
  return (
    <ToastProvider>
      <AuthProvider>
        <AppRoutes />
      </AuthProvider>
    </ToastProvider>
  )
}
