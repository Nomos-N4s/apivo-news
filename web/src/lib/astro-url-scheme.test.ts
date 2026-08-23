import { execFileSync } from 'node:child_process';
import { createRequire, } from 'node:module';
import { mkdtempSync, readFileSync } from 'node:fs';
import http from 'node:http';
import https from 'node:https';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { pathToFileURL } from 'node:url';

import { describe, expect, it } from 'vitest';

/**
 * The contract this deployment's TLS hop exists to satisfy.
 *
 * @astrojs/node derives the request URL's scheme from the SOCKET and nothing
 * else (astro/dist/core/app/node.js):
 *
 *     const isEncrypted = 'encrypted' in req.socket && req.socket.encrypted;
 *     const protocol = isEncrypted ? 'https' : 'http';
 *
 * X-Forwarded-Proto is not consulted, in standalone or middleware mode.
 * Astro's own CSRF middleware then compares that URL's origin against the
 * browser's Origin header with ===, so a frontend served over plain http
 * behind a TLS terminator refuses every form post the site makes of itself:
 * sign-in, approval, withdrawal, source registration.
 *
 * deploy/hetzner answers that by giving the container a certificate -
 * provision.sh writes it, compose mounts it, Caddy proxies over https and
 * does not verify it. This test is what says that answer is still needed.
 *
 * It reaches into an INTERNAL path of a dependency deliberately, and has to
 * go around the package's own `exports` map to do it - resolving
 * astro/package.json, which IS exported, and walking from there. That is normally a mistake and here it is the point: the
 * behaviour is undocumented, no public surface expresses it, and a
 * deployment decision rests on it. If a future Astro honours
 * X-Forwarded-Proto or changes how it reads the socket, this fails - and the
 * TLS hop should then be reconsidered rather than carried forward out of
 * habit. A failure here is news, not breakage.
 */

const require = createRequire(import.meta.url);

interface NodeAppExports {
  createRequestFromNodeRequest: (req: unknown, opts: { skipBody: boolean }) => Request;
}

async function astroNodeApp(): Promise<NodeAppExports> {
  const root = dirname(require.resolve('astro/package.json'));
  const entry = pathToFileURL(join(root, 'dist/core/app/node.js')).href;
  return (await import(/* @vite-ignore */ entry)) as NodeAppExports;
}

/** A throwaway certificate; Node can make a key pair but not sign an X.509. */
function selfSigned(): { key: string; cert: string } {
  const dir = mkdtempSync(`${tmpdir()}/astro-url-scheme-`);
  execFileSync(
    'openssl',
    ['req', '-new', '-x509', '-days', '1', '-nodes', '-out', `${dir}/c.crt`, '-keyout', `${dir}/c.key`, '-subj', '/CN=localhost'],
    { stdio: 'ignore' },
  );
  return { key: readFileSync(`${dir}/c.key`, 'utf8'), cert: readFileSync(`${dir}/c.crt`, 'utf8') };
}

/**
 * The origin Astro would compute for a request arriving on this server, with
 * the Host header a proxy passes through untouched.
 */
async function originSeenBy(server: http.Server | https.Server): Promise<string> {
  const { createRequestFromNodeRequest } = await astroNodeApp();
  return new Promise((resolve, reject) => {
    server.on('request', (req, res) => {
      const request = createRequestFromNodeRequest(req, { skipBody: true });
      res.writeHead(200);
      res.end(new URL(request.url).origin);
    });
    server.on('error', reject);
    // Port 0: the OS picks a free one, so two of these never collide and
    // the suite does not depend on what else this machine is running.
    server.listen(0, '127.0.0.1', () => {
      const address = server.address();
      if (address === null || typeof address === 'string') {
        reject(new Error('the probe server has no port'));
        return;
      }
      const mod = server instanceof https.Server ? https : http;
      const req = mod.request(
        {
          host: '127.0.0.1',
          port: address.port,
          path: '/el/editor/signin',
          method: 'POST',
          headers: { host: 'pr-146.example.invalid' },
          rejectUnauthorized: false,
        },
        (res) => {
          let body = '';
          res.on('data', (chunk) => (body += chunk));
          res.on('end', () => {
            server.close();
            resolve(body);
          });
        },
      );
      req.on('error', reject);
      req.end();
    });
  });
}

describe('the scheme Astro reports comes from the socket', () => {
  // The bug, stated as a fact rather than as a story: this is the value
  // Astro's origin check compares the browser's https:// Origin against,
  // and === says no.
  it('a plain http server reports http, whatever the browser used', async () => {
    await expect(originSeenBy(http.createServer())).resolves.toBe('http://pr-146.example.invalid');
  });

  it('a TLS server reports https, which is what makes the origin check agree', async () => {
    await expect(originSeenBy(https.createServer(selfSigned()))).resolves.toBe('https://pr-146.example.invalid');
  });
});
