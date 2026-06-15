// Fleet-scoped live event client over WebSocket.

type EventHandler = (type: string) => void;

export class WSClient {
  private url: string;
  private ws: WebSocket | null = null;
  private handlers = new Set<EventHandler>();
  private reconnectHandlers = new Set<() => void>();
  private closed = false;
  private connectedOnce = false;
  private backoff = 1000;

  constructor(fleet: string) {
    const proto = location.protocol === "https:" ? "wss" : "ws";
    this.url = `${proto}://${location.host}/api/ws?fleet=${fleet}`;
  }

  onEvent(h: EventHandler) {
    this.handlers.add(h);
    return () => this.handlers.delete(h);
  }
  onReconnect(h: () => void) {
    this.reconnectHandlers.add(h);
    return () => this.reconnectHandlers.delete(h);
  }

  connect() {
    if (this.closed) return;
    const ws = new WebSocket(this.url);
    this.ws = ws;
    ws.onopen = () => {
      const wasReconnect = this.connectedOnce;
      this.connectedOnce = true;
      this.backoff = 1000;
      if (wasReconnect) this.reconnectHandlers.forEach((h) => h());
    };
    ws.onmessage = (e) => {
      try {
        const m = JSON.parse(e.data as string);
        if (m && typeof m.t === "string") this.handlers.forEach((h) => h(m.t));
      } catch {
        /* ignore non-JSON frames */
      }
    };
    ws.onclose = () => {
      if (this.closed) return;
      setTimeout(() => this.connect(), this.backoff);
      this.backoff = Math.min(this.backoff * 2, 15000);
    };
    ws.onerror = () => ws.close();
  }

  close() {
    this.closed = true;
    this.ws?.close();
  }
}
