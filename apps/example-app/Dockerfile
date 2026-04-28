# syntax=docker/dockerfile:1.7

# Stage 1 - install workspace dependencies (the build context is the
# repo root so the lockfile resolves the full workspace).
FROM node:24-alpine AS deps
WORKDIR /repo
RUN corepack enable
COPY package.json pnpm-lock.yaml .npmrc ./
COPY libs/gateway-sdk/package.json ./libs/gateway-sdk/
COPY apps/example-app/package.json ./apps/example-app/
RUN --mount=type=cache,id=pnpm,target=/root/.local/share/pnpm/store \
    pnpm install --frozen-lockfile --ignore-scripts

# Stage 2 - bundle example-app via Nx webpack.
FROM deps AS build
COPY tsconfig.base.json nx.json jest.preset.js jest.config.ts ./
COPY libs ./libs
COPY apps/example-app ./apps/example-app
RUN node_modules/.bin/nx build example-app --configuration=production

# Stage 3 - produce a self-contained runtime bundle. We re-install
# production deps with npm in a fresh tree so the runtime gets a
# flat, symlink-free node_modules that survives a Docker COPY into
# distroless. pnpm's hoisted layout still uses .pnpm symlinks that
# Node 24 follows to realpath, breaking module resolution from
# inside packages whose deps aren't physically nested.
FROM node:24-alpine AS prod-deps
WORKDIR /bundle
COPY apps/example-app/package.json ./
RUN npm install --omit=dev --no-audit --no-fund --legacy-peer-deps

# Stage 4 - distroless runtime.
FROM gcr.io/distroless/nodejs24-debian12 AS runtime
WORKDIR /app
ENV NODE_ENV=production
COPY --from=build /repo/dist/apps/example-app /app
COPY --from=prod-deps /bundle/node_modules /app/node_modules
EXPOSE 3001
CMD ["main.js"]
