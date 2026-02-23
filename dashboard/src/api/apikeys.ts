import { apikeyClient } from './client'
import type { ApiKey, CreateKeyRequest, CreateKeyResponse } from '../types/apikey'

export const apikeysApi = {
  listKeys: () =>
    apikeyClient.get('v1/keys').json<{ keys: ApiKey[]; count: number }>(),

  createKey: (req: CreateKeyRequest) =>
    apikeyClient.post('v1/keys', { json: req }).json<CreateKeyResponse>(),

  revokeKey: (id: string) =>
    apikeyClient.delete(`v1/keys/${id}`).json<{ status: string }>(),
}
