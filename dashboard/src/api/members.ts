import { fleetClient } from './client'
import type { OrgMember, InviteMemberRequest, UpdateAccessRequest } from '../types/member'

export const membersApi = {
  listMembers: () =>
    fleetClient.get('v1/org/members').json<{ members: OrgMember[]; count: number }>(),

  inviteMember: (req: InviteMemberRequest) =>
    fleetClient.post('v1/org/invite', { json: req }).json<{ id: string; email: string; role: string }>(),

  updateMemberAccess: (userId: string, req: UpdateAccessRequest) =>
    fleetClient.put(`v1/org/members/${userId}/access`, { json: req }).json<{ status: string }>(),

  removeMember: (userId: string) =>
    fleetClient.delete(`v1/org/members/${userId}`).json<{ status: string }>(),
}
