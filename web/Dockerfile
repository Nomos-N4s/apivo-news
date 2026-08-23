# Build stage: install exactly the locked dependency tree, produce the
# standalone server bundle (@astrojs/node, mode "standalone"), then prune
# devDependencies — the bundle still imports Astro's runtime helpers from
# node_modules, so the pruned production tree ships with it.
FROM node:24-slim AS build
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci
COPY . .
RUN npm run build && npm prune --omit=dev

# Runtime stage: minimal Node image carrying only the built output and
# production dependencies, running as the unprivileged "node" user that
# ships with the official image.
FROM node:24-slim AS runtime
ENV NODE_ENV=production \
    HOST=0.0.0.0 \
    PORT=4321
WORKDIR /app
COPY --from=build --chown=node:node /app/node_modules ./node_modules
COPY --from=build --chown=node:node /app/dist ./dist
USER node
EXPOSE 4321
# curl is absent from the slim image; Node's built-in fetch probes the
# standalone server instead. Exec form, so no shell is required either.
#
# The SCHEME follows the server rather than assuming: @astrojs/node's
# standalone entry serves https when SERVER_CERT_PATH and SERVER_KEY_PATH
# are both set (dist/standalone.js), and this deployment sets them so that
# Astro.url carries the scheme the browser used. A probe hardcoded to http
# would fail against that server, and a failing HEALTHCHECK is not a warning
# - compose --wait refuses to finish, so every deploy and every preview
# would hang on a container that was serving perfectly.
#
# The certificate is self-signed and presented to a client one hop away on a
# private network, so the probe does not verify it. It is checking that this
# process answers, not who it is.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["node", "-e", "const tls = process.env.SERVER_CERT_PATH && process.env.SERVER_KEY_PATH; const url = (tls ? 'https' : 'http') + '://localhost:' + (process.env.PORT || 4321) + '/'; if (tls) process.env.NODE_TLS_REJECT_UNAUTHORIZED = '0'; fetch(url).then((r) => process.exit(r.ok ? 0 : 1)).catch(() => process.exit(1))"]
CMD ["node", "dist/server/entry.mjs"]
