/**
 * WsManager — singleton WebSocket connection to ws-gateway.
 *
 * Not a React component. Connects once per unique subscription set,
 * handles exponential-backoff reconnect, dispatches frames to stores.
 *
 * Subscribe request format (sent as JSON after connect):
 *   { vehicle_ids: string[], streams: string[], format: "json" }
 *
 * Frame types (from ws-gateway with format=json):
 *   { type: "telemetry", vehicle_id, ts, lat, lon, alt, seq, payload_b64 }
 *   { type: "alert", device, level, message }
 *   { type: "assistance_request", device, request_id, reason }
 *
 * Usage (ref-counted per vehicle):
 *   wsManager.subscribe(vehicleId)   // adds to set, reconnects if changed
 *   wsManager.unsubscribe(vehicleId) // removes; disconnects when set is empty
 */

import { env } from '../env'
import { useAuthStore } from '../store/authStore'
import { useTelemetryStore } from '../store/telemetryStore'
import { useAlertStore }     from '../store/alertStore'
import type { LiveFrame }    from '../types/telemetry'

export type WsStatus = 'disconnected' | 'connecting' | 'connected'

type StatusListener = (s: WsStatus) => void

class WsManager {
  private ws: WebSocket | null = null
  private _subscribedIds: Set<string> = new Set()
  private _streams = ['telemetry', 'event', 'alert', 'assistance']
  private retryDelay = 1000
  private retryTimer: ReturnType<typeof setTimeout> | null = null
  private destroyed  = false
  private _status: WsStatus = 'disconnected'
  private _listeners: Set<StatusListener> = new Set()

  // ── Status observable (for TopBar LED) ─────────────────────────────────────

  get status(): WsStatus { return this._status }

  onStatusChange(fn: StatusListener): () => void {
    this._listeners.add(fn)
    return () => this._listeners.delete(fn)
  }

  private _setStatus(s: WsStatus): void {
    if (this._status === s) return
    this._status = s
    this._listeners.forEach((fn) => fn(s))
  }

  // ── Public API — ref-counted per vehicle ────────────────────────────────────

  /** Add one vehicle to the subscription set. Reconnects if the set changed. */
  subscribe(vehicleId: string): void {
    if (this._subscribedIds.has(vehicleId)) return
    this._subscribedIds.add(vehicleId)
    this.destroyed = false
    this._reconnect()
  }

  /** Remove one vehicle. Disconnects when the set becomes empty. */
  unsubscribe(vehicleId: string): void {
    if (!this._subscribedIds.has(vehicleId)) return
    this._subscribedIds.delete(vehicleId)
    if (this._subscribedIds.size === 0) {
      this._teardown()
    } else {
      this._reconnect()
    }
  }

  // ── Internal ───────────────────────────────────────────────────────────────

  private _teardown(): void {
    this.destroyed = true
    this.retryDelay = 1000
    if (this.retryTimer) { clearTimeout(this.retryTimer); this.retryTimer = null }
    if (this.ws) { this.ws.onclose = null; this.ws.close(); this.ws = null }
    this._setStatus('disconnected')
  }

  /** Close any open socket and open a fresh one with the current ID set. */
  private _reconnect(): void {
    if (this.retryTimer) { clearTimeout(this.retryTimer); this.retryTimer = null }
    if (this.ws && this.ws.readyState !== WebSocket.CLOSED) {
      this.ws.onclose = null  // suppress the retry-loop that onclose would trigger
      this.ws.close()
      this.ws = null
    }
    this._open()
  }

  private _open(): void {
    if (this.destroyed || this._subscribedIds.size === 0) return

    this._setStatus('connecting')
    const token = useAuthStore.getState().token ?? ''
    const url   = `${env.wsGatewayUrl}/ws?token=${token}`

    this.ws = new WebSocket(url)

    this.ws.onopen = () => {
      this.retryDelay = 1000
      this._setStatus('connected')
      this.ws?.send(JSON.stringify({
        vehicle_ids: [...this._subscribedIds],
        streams:     this._streams,
        format:      'json',
      }))
    }

    this.ws.onmessage = ({ data }) => {
      if (typeof data !== 'string') return
      let frame: LiveFrame
      try { frame = JSON.parse(data) }
      catch { return }

      if (frame.type === 'telemetry') {
        useTelemetryStore.getState().push(frame.vehicle_id, frame)
      } else if (frame.type === 'alert') {
        useAlertStore.getState().push(frame)
      } else if (frame.type === 'assistance_request') {
        useAlertStore.getState().push({
          type: 'alert',
          device: frame.device,
          level: 'warning',
          message: `Assistance requested: ${frame.reason}`,
        })
      }
    }

    this.ws.onerror  = () => { /* onclose fires after onerror */ }
    this.ws.onclose  = () => {
      this._setStatus('disconnected')
      this._scheduleReconnect()
    }
  }

  private _scheduleReconnect(): void {
    if (this.destroyed || this._subscribedIds.size === 0) return
    this._setStatus('connecting')
    this.retryTimer = setTimeout(() => {
      this.retryDelay = Math.min(this.retryDelay * 2, 30_000)
      this._open()
    }, this.retryDelay)
  }
}

export const wsManager = new WsManager()
