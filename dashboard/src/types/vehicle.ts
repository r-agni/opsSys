export interface Vehicle {
  id: string
  org_id: string
  project_id?: string
  display_name: string
  vehicle_type: string
  adapter_type: string
  region: string
  created_at: string
  updated_at: string
  active: boolean
}

export interface Project {
  id: string
  org_id: string
  name: string
  display_name: string
  created_at: string
  active: boolean
}

export interface ProvisionResponse {
  device_id: string
  agent_config_yaml: string
  cert_pem?: string
  key_pem?: string
  ca_pem?: string
}
