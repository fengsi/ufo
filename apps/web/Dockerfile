# Build context is apps/web. The browser talks to the API same-origin: Next
# proxies /api to API_PROXY_TARGET at runtime (see next.config.mjs), so nothing
# API-related is baked at build time.
FROM node:26-alpine AS build
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci
COPY . .
# Next bakes rewrite destinations at build time, so the proxy target is a build
# arg (defaults to the compose service name). Host `next dev` re-reads it per-run.
ARG API_PROXY_TARGET=http://api:8080
ENV API_PROXY_TARGET=$API_PROXY_TARGET
RUN npm run build

FROM node:26-alpine AS run
WORKDIR /app
ENV NODE_ENV=production
COPY --from=build /app/package.json ./package.json
COPY --from=build /app/node_modules ./node_modules
COPY --from=build /app/.next ./.next
COPY --from=build /app/next.config.mjs ./next.config.mjs
EXPOSE 3000
CMD ["npm", "run", "start"]
