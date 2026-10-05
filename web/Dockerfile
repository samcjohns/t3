FROM node:22-alpine AS build
WORKDIR /src
COPY package.json package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY . .
RUN npm run build

FROM nginxinc/nginx-unprivileged:1.29-alpine
COPY nginx/ /etc/nginx/templates/
COPY --from=build /src/dist /usr/share/nginx/html
ENV T3_API_URL=http://localhost:8080
EXPOSE 8080
