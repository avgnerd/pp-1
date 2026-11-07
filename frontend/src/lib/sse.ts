// Minimal Server-Sent Events subscription with JSON payloads. Returns a close function.

// Payloads are server JSON; each handler reads the fields it knows.
// eslint-disable-next-line @typescript-eslint/no-explicit-any
type Payload = any

export function subscribe(url: string, handlers: Record<string, (data: Payload) => void>, onError?: () => void): () => void {
  const es = new EventSource(url, { withCredentials: true })
  for (const [event, fn] of Object.entries(handlers)) {
    es.addEventListener(event, (e) => {
      let data: Payload
      try {
        data = JSON.parse((e as MessageEvent).data)
      } catch {
        return
      }
      fn(data)
    })
  }
  es.onerror = () => {
    // The server closes the stream when it finishes; EventSource would reconnect forever.
    es.close()
    onError?.()
  }
  return () => es.close()
}
