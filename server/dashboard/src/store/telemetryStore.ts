import { create } from 'zustand'
import { CircularBuffer } from '../ws/CircularBuffer'
import type { TelemetryFrame } from '../types/telemetry'

const CAPACITY = 1000  // ~20s at 50Hz

interface TelemetryState {
  /** Monotonically increasing counter — RAF loops read this to detect new data */
  generation: number
  buffers: Map<string, CircularBuffer<TelemetryFrame>>
  push:      (vehicleId: string, frame: TelemetryFrame) => void
  getBuffer: (vehicleId: string) => CircularBuffer<TelemetryFrame> | undefined
  reset:     (vehicleId: string) => void
}

export const useTelemetryStore = create<TelemetryState>((set, get) => ({
  generation: 0,
  buffers:    new Map(),

  push: (vehicleId, frame) => {
    const { buffers, generation } = get()
    let buf = buffers.get(vehicleId)
    if (!buf) {
      buf = new CircularBuffer<TelemetryFrame>(CAPACITY)
      buffers.set(vehicleId, buf)
    }
    buf.push(frame)
    set({ generation: generation + 1 })
  },

  getBuffer: (vehicleId) => get().buffers.get(vehicleId),

  reset: (vehicleId) => {
    const { buffers } = get()
    buffers.delete(vehicleId)
    set({ generation: get().generation + 1 })
  },
}))
