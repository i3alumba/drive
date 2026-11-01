FROM node:22-bookworm-slim@sha256:9f0dde62d10e7ff7a310ecaebd66225f6b46c17e6cb778449a77b05d3541bea2 AS build
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci
COPY . .
RUN npm run build

FROM nginx:1.27-alpine
COPY --from=build /app/dist /usr/share/nginx/html
COPY nginx.conf /etc/nginx/conf.d/default.conf
EXPOSE 80
