import { fleetClient } from './client'
import type { Vehicle, Project, ProvisionResponse } from '../types/vehicle'

export const fleetApi = {
  listVehicles: () =>
    fleetClient.get('v1/vehicles').json<{ vehicles: Vehicle[]; count: number }>(),

  getVehicle: (id: string) =>
    fleetClient.get(`v1/vehicles/${id}`).json<Vehicle>(),

  listProjects: () =>
    fleetClient.get('v1/projects').json<{ projects: Project[]; count: number }>(),

  createProject: (name: string, displayName?: string) =>
    fleetClient.post('v1/projects', {
      json: { name, display_name: displayName ?? name },
    }).json<Project>(),

  listProjectDevices: (projectName: string) =>
    fleetClient.get(`v1/projects/${projectName}/devices`).json<{ devices: Vehicle[]; count: number }>(),

  provisionDevice: (projectName: string, body: {
    display_name: string
    vehicle_type?: string
    adapter_type?: string
    region?: string
  }) =>
    fleetClient.post(`v1/projects/${projectName}/devices`, { json: body }).json<ProvisionResponse>(),
}
