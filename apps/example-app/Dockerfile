# syntax=docker/dockerfile:1.7

# Stage 1 - install workspace dependencies (the build context is the
# repo root so the lockfile resolves the full workspace).
FROM node:20-alpine AS deps
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

# Stage 3 - distroless runtime.
FROM gcr.io/distroless/nodejs20-debian12 AS runtime
WORKDIR /app
ENV NODE_ENV=production
COPY --from=build /repo/dist/apps/example-app /app
COPY --from=build /repo/node_modules /app/node_modules
EXPOSE 3001
CMD ["main.js"]
