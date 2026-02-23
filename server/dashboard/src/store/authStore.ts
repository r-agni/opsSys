import { create } from 'zustand'
import { UserManager, User, WebStorageStateStore } from 'oidc-client-ts'
import { env } from '../env'

// Keycloak OIDC PKCE — no client secret, browser-safe
const userManager = new UserManager({
  authority:              `${env.keycloakUrl}/realms/${env.keycloakRealm}`,
  client_id:              env.keycloakClientId,
  redirect_uri:           `${window.location.origin}/auth/callback`,
  post_logout_redirect_uri: `${window.location.origin}/login`,
  response_type:          'code',
  scope:                  'openid profile email',
  automaticSilentRenew:   true,
  silent_redirect_uri:    `${window.location.origin}/silent-renew.html`,
  userStore:              new WebStorageStateStore({ store: window.localStorage }),
})

export interface AuthClaims {
  org_id: string
  vehicle_set: string[]
  role: 'viewer' | 'operator' | 'admin'
  sub: string
  email?: string
}

interface AuthState {
  user: User | null
  token: string | null
  claims: AuthClaims | null
  isAdmin:    () => boolean
  canCommand: () => boolean
  login:      () => void
  signup:     () => void
  logout:     () => void
  handleCallback: () => Promise<void>
  loadExisting:   () => Promise<void>
}

function parseClaims(user: User): AuthClaims {
  const p = user.profile as Record<string, unknown>
  return {
    org_id:      (p['org_id']      as string)   ?? '',
    vehicle_set: (p['vehicle_set'] as string[]) ?? [],
    role:        (p['role']        as AuthClaims['role']) ?? 'viewer',
    sub:         user.profile.sub ?? '',
    email:       user.profile.email,
  }
}

export const useAuthStore = create<AuthState>((set, get) => ({
  user:   null,
  token:  null,
  claims: null,

  isAdmin:    () => get().claims?.role === 'admin',
  canCommand: () => ['operator', 'admin'].includes(get().claims?.role ?? ''),

  login: () => {
    userManager.signinRedirect()
  },

  signup: () => {
    userManager.signinRedirect({ extraQueryParams: { kc_action: 'register' } })
  },

  logout: () => {
    userManager.signoutRedirect()
    set({ user: null, token: null, claims: null })
  },

  handleCallback: async () => {
    const user = await userManager.signinRedirectCallback()
    set({ user, token: user.access_token, claims: parseClaims(user) })
  },

  loadExisting: async () => {
    try {
      const user = await userManager.getUser()
      if (user && !user.expired) {
        set({ user, token: user.access_token, claims: parseClaims(user) })
      }
    } catch {
      // no stored session
    }
  },
}))

// Keep zustand store in sync when oidc-client-ts refreshes tokens
userManager.events.addUserLoaded((user) => {
  useAuthStore.setState({
    user,
    token: user.access_token,
    claims: parseClaims(user),
  })
})

userManager.events.addAccessTokenExpired(() => {
  userManager.signinSilent().catch(() => {
    useAuthStore.setState({ user: null, token: null, claims: null })
  })
})

userManager.events.addSilentRenewError(() => {
  userManager.signinSilent().catch(() => {
    useAuthStore.setState({ user: null, token: null, claims: null })
  })
})

// Re-export userManager so WsManager can get the token
export { userManager }
