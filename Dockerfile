FROM golang:alpine AS builder
WORKDIR /app
COPY . /app
RUN go build -o reprox .

FROM nginx:alpine
RUN apk add --no-cache certbot certbot-nginx bash openssl && rm /etc/nginx/conf.d/default.conf
COPY --from=builder /app/reprox /usr/bin/reprox
VOLUME ["/etc/letsencrypt"]
EXPOSE 80 443
COPY nginx.conf /etc/nginx/nginx.conf
ENTRYPOINT ["/usr/bin/reprox"]
