/**
 * ky HTTP client factory.
 * Injects Authorization: Bearer header from authStore.
 * On 401: triggers logout so user gets redirected to login.
 */
import ky from 'ky'
import { useAuthStore } from '../store/authStore'

const authHooks = {
  beforeRequest: [
    (req: Request) => {
      const token = useAuthStore.getState().token
      if (token) req.headers.set('Authorization', `Bearer ${token}`)
    },
  ],
  afterResponse: [
    (_req: Request, _opts: unknown, res: Response) => {
      if (res.status === 401) useAuthStore.getState().logout()
    },
  ],
}

export const fleetClient = ky.create({
  prefixUrl: import.meta.env.VITE_FLEET_API_URL ?? 'http://localhost:8080',
  hooks: authHooks,
})

export const apikeyClient = ky.create({
  prefixUrl: import.meta.env.VITE_APIKEY_URL ?? 'http://localhost:8083',
  hooks: authHooks,
})
