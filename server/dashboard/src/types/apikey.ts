export interface ApiKey {
  id: string
  key_prefix: string
  org_id: string
  name: string
  scopes: string[]
  created_at: string
  expires_at?: string
  revoked_at?: string
  last_used_at?: string
}

export interface CreateKeyRequest {
  name: string
  scopes: string[]
  expires_at?: string
}

export interface CreateKeyResponse extends ApiKey {
  api_key: string  // only returned once on creation
}
