import { createServer, request, type OutgoingHttpHeaders } from 'node:http';
import { type AddressInfo, connect, type Socket } from 'node:net';

/**
 * A forward HTTP proxy for the browser contexts of the distribution specs
 * (TD-D-01..03): the browser asks for http://www.shop.test/..., the proxy
 * sends the request to the S3 listener of the server under test keeping
 * the domain in the Host header, as a DNS record pointing the domain to
 * the cluster would. Only the domains ending with the suffix are served
 * (HTTP, port 80): any other host is refused, nothing leaves the host.
 */
export interface HostProxy {
  /** proxy.server of a browser context. */
  server: string;
  /** Host header of every forwarded request, in order. */
  hosts: string[];
  close(): Promise<void>;
}

/** Hop-by-hop headers a proxy does not forward. */
const hopByHop = new Set([
  'connection',
  'keep-alive',
  'proxy-authorization',
  'proxy-connection',
  'te',
  'trailer',
  'transfer-encoding',
  'upgrade',
]);

/** Starts the proxy on a free loopback port; target is the origin of the S3 listener. */
export async function startHostProxy(target: string, suffix = '.test'): Promise<HostProxy> {
  const upstream = new URL(target);
  const hosts: string[] = [];
  const tunnels = new Set<Socket>();
  const server = createServer((req, res) => {
    let url: URL;
    try {
      // A request to a proxy carries the absolute URL (absolute-form).
      url = new URL(req.url ?? '');
    } catch {
      res.writeHead(400).end('absolute URL expected');
      return;
    }
    if (url.protocol !== 'http:' || !url.hostname.endsWith(suffix)) {
      res.writeHead(502).end(`not a test domain: ${url.host}`);
      return;
    }
    hosts.push(url.host);
    const headers: OutgoingHttpHeaders = {};
    for (const [name, value] of Object.entries(req.headers)) {
      if (!hopByHop.has(name) && value !== undefined) headers[name] = value;
    }
    headers.host = url.host;
    const forward = request(
      {
        hostname: upstream.hostname,
        port: upstream.port,
        method: req.method,
        path: `${url.pathname}${url.search}`,
        headers,
      },
      (answer) => {
        const out: OutgoingHttpHeaders = {};
        for (const [name, value] of Object.entries(answer.headers)) {
          if (!hopByHop.has(name) && value !== undefined) out[name] = value;
        }
        res.writeHead(answer.statusCode ?? 502, out);
        answer.pipe(res);
      },
    );
    forward.on('error', (error) => {
      if (!res.headersSent) res.writeHead(502);
      res.end(String(error));
    });
    req.pipe(forward);
  });
  // Tunnels (the API requests of Playwright use them even for http://):
  // to the S3 listener for port 80 of a test domain, the Host header of
  // the tunnelled request is the domain.
  server.on('connect', (req, socket: Socket, head) => {
    const [host = '', port = '80'] = (req.url ?? '').split(':');
    if (!host.endsWith(suffix) || port !== '80') {
      socket.end('HTTP/1.1 502 Bad Gateway\r\n\r\n');
      return;
    }
    hosts.push(host);
    const tunnel = connect(Number(upstream.port), upstream.hostname, () => {
      socket.write('HTTP/1.1 200 Connection Established\r\n\r\n');
      tunnel.write(head);
      tunnel.pipe(socket);
      socket.pipe(tunnel);
    });
    tunnels.add(socket).add(tunnel);
    const close = () => {
      tunnel.destroy();
      socket.destroy();
      tunnels.delete(socket);
      tunnels.delete(tunnel);
    };
    tunnel.on('error', close).on('close', close);
    socket.on('error', close).on('close', close);
  });
  await new Promise<void>((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, '127.0.0.1', () => {
      resolve();
    });
  });
  const { port } = server.address() as AddressInfo;
  return {
    server: `http://127.0.0.1:${String(port)}`,
    hosts,
    close: () =>
      new Promise<void>((resolve) => {
        for (const socket of tunnels) socket.destroy();
        server.closeAllConnections();
        server.close(() => {
          resolve();
        });
      }),
  };
}
