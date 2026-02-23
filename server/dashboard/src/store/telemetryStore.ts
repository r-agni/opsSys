import { create } from 'zustand'
import { CircularBuffer } from '../ws/CircularBuffer'
import type { TelemetryFrame } from '../types/telemetry'

const CAPACITY = 1000  // ~20s at 50Hz

interface TelemetryState {
  /** Monotonically increasing counter — components read this to detect new data */
  generation: number
  buffers: Map<string, CircularBuffer<TelemetryFrame>>
  push:      (vehicleId: string, frame: TelemetryFrame) => void
  getBuffer: (vehicleId: string) => CircularBuffer<TelemetryFrame> | undefined
  reset:     (vehicleId: string) => void
}

let _rafScheduled = false
let _dirty = false

function scheduleFlush() {
  if (_rafScheduled) return
  _rafScheduled = true
  requestAnimationFrame(() => {
    _rafScheduled = false
    if (_dirty) {
      _dirty = false
      useTelemetryStore.setState((s) => ({ generation: s.generation + 1 }))
    }
  })
}

export const useTelemetryStore = create<TelemetryState>((set, get) => ({
  generation: 0,
  buffers:    new Map(),

  push: (vehicleId, frame) => {
    const { buffers } = get()
    let buf = buffers.get(vehicleId)
    if (!buf) {
      buf = new CircularBuffer<TelemetryFrame>(CAPACITY)
      buffers.set(vehicleId, buf)
    }
    buf.push(frame)
    _dirty = true
    scheduleFlush()
  },

  getBuffer: (vehicleId) => get().buffers.get(vehicleId),

  reset: (vehicleId) => {
    const { buffers } = get()
    buffers.delete(vehicleId)
    set({ generation: get().generation + 1 })
  },
}))
