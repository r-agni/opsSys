export interface OrgMember {
  id: string
  email: string
  name: string
  role: string
  device_ids: string[]
}

export interface InviteMemberRequest {
  email: string
  role: string
  device_ids: string[]
}

export interface UpdateAccessRequest {
  role: string
  device_ids: string[]
}
