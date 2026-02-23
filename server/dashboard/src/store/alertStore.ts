import { create } from 'zustand'
import type { AlertFrame } from '../types/telemetry'

const MAX_ALERTS = 200

interface AlertState {
  alerts: AlertFrame[]
  push:   (alert: AlertFrame) => void
  clear:  () => void
}

export const useAlertStore = create<AlertState>((set) => ({
  alerts: [],
  push: (alert) =>
    set((s) => ({
      alerts: [alert, ...s.alerts].slice(0, MAX_ALERTS),
    })),
  clear: () => set({ alerts: [] }),
}))
