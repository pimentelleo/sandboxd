# Build the SPA, then serve it from nginx with /v1 proxied to sandboxd.
FROM node:20-slim AS build
RUN corepack enable
WORKDIR /app
COPY package.json pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile
COPY . .
RUN pnpm build

FROM nginx:stable-alpine
# The official nginx entrypoint renders this template after discovering the
# container network's DNS resolver from /etc/resolv.conf.
ENV NGINX_ENTRYPOINT_LOCAL_RESOLVERS=1
COPY nginx.conf /etc/nginx/templates/default.conf.template
COPY --from=build /app/dist /usr/share/nginx/html
EXPOSE 80
