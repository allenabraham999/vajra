import { Navigate, Route, Routes } from 'react-router-dom'
import { AuthProvider, useAuth } from './auth/AuthContext'
import { ToastProvider } from './components/Toast'
import Layout from './components/Layout'
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

function RequireAuth({ children }: { children: React.ReactNode }) {
  const { token } = useAuth()
  if (!token) return <Navigate to="/login" replace />
  return <>{children}</>
}

function AppRoutes() {
  return (
    <Routes>
      <Route path="/login" element={<LoginPage />} />
      <Route
        path="/"
        element={
          <RequireAuth>
            <Layout />
          </RequireAuth>
        }
      >
        <Route index element={<Dashboard />} />
        <Route path="sandboxes" element={<SandboxesPage />} />
        <Route path="sandboxes/:id" element={<SandboxDetailPage />} />
        <Route path="templates" element={<TemplatesPage />} />
        <Route path="nodes" element={<NodesPage />} />
        <Route path="api-keys" element={<ApiKeysPage />} />
        <Route path="usage" element={<UsagePage />} />
        <Route path="metrics" element={<MetricsPage />} />
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
