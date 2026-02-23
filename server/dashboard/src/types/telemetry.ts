export interface TelemetryRow {
  vehicle_id: string
  timestamp: string
  stream_type: string
  lat: number
  lon: number
  alt_m: number
  seq_num: number
  payload_size: number
}

/** Live JSON frame from ws-gateway (format=json mode) */
export type LiveFrame =
  | TelemetryFrame
  | AlertFrame
  | AssistanceFrame

export interface TelemetryFrame {
  type: 'telemetry'
  vehicle_id: string
  ts: number        // nanoseconds UTC
  lat: number
  lon: number
  alt: number
  seq: number
  payload_b64: string
}

export interface AlertFrame {
  type: 'alert'
  device: string
  level: 'info' | 'warning' | 'error' | 'critical'
  message: string
}

export interface AssistanceFrame {
  type: 'assistance_request'
  device: string
  request_id: string
  reason: string
}
