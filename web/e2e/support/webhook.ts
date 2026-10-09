import { createServer, type IncomingMessage, type Server } from 'node:http';

/** A request received by the webhook of a test. */
export interface WebhookDelivery {
  headers: IncomingMessage['headers'];
  body: unknown;
}

/** The S3 event name of a delivery (its first record, or EventName). */
export function eventNameOf(delivery: WebhookDelivery): string | undefined {
  const body = delivery.body as { EventName?: string; Records?: { eventName?: string }[] };
  return body.Records?.[0]?.eventName ?? body.EventName;
}

/** An HTTP server of the test receiving the event notifications of Nexo. */
export interface Webhook {
  /** The endpoint to configure in notify_webhook. */
  url: string;
  deliveries: WebhookDelivery[];
  close(): Promise<void>;
}

/**
 * Starts a webhook on a free loopback port: every POST is recorded (its
 * JSON body) and answered 200, the way a notification endpoint acknowledges
 * an event.
 */
export function startWebhook(): Promise<Webhook> {
  const deliveries: WebhookDelivery[] = [];
  const server: Server = createServer((req, res) => {
    const chunks: Buffer[] = [];
    req.on('data', (chunk: Buffer) => chunks.push(chunk));
    req.on('end', () => {
      const text = Buffer.concat(chunks).toString('utf8');
      let body: unknown = text;
      try {
        body = text === '' ? null : (JSON.parse(text) as unknown);
      } catch {
        // Kept as text.
      }
      if (req.method === 'POST') {
        deliveries.push({ headers: req.headers, body });
      }
      res.writeHead(200, { 'content-type': 'text/plain' });
      res.end('ok');
    });
  });
  return new Promise((resolve, reject) => {
    server.on('error', reject);
    server.listen(0, '127.0.0.1', () => {
      const address = server.address();
      const port = typeof address === 'object' && address !== null ? address.port : 0;
      resolve({
        url: `http://127.0.0.1:${String(port)}/events`,
        deliveries,
        close: () =>
          new Promise<void>((done) => {
            server.closeAllConnections();
            server.close(() => {
              done();
            });
          }),
      });
    });
  });
}
