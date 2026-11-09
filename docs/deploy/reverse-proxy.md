# TLS behind a reverse proxy

<!-- commands checked by: scripts/deploy-compose-gate.sh -->

Sindri speaks plain HTTP and expects something in front of it to terminate TLS. A registry
asks more of a proxy than a website does, and the same four things matter on every proxy:

- **No limit on the request body.** One image layer can be several gigabytes.
- **No buffering, in either direction.** Buffering writes every upload and download to the
  proxy's disk first.
- **Long timeouts.** A large layer takes minutes. The examples allow 15 minutes on nginx
  and none on Traefik. Set a limit that covers your largest layer on your slowest link.
- **Send the original `Host` header,** because a client follows the upload location
  Sindri returns.

Hide `/metrics`, `/healthz`, and `/readyz`. They carry no authentication, and each example
answers 404 for them. Scrape metrics from inside your network instead.

Sindri does not support cross-repository mounts, so pushing a layer that the server
already holds still uploads every byte.

The examples reach Sindri as `sindri:8080`, its name on a shared Compose network. Use
`127.0.0.1:8080` if the proxy runs on the host.

## nginx

<!-- proxy:nginx -->
```nginx
server {
    listen 443 ssl;
    server_name sindri.example.com;
    ssl_certificate     /etc/nginx/tls/fullchain.pem;
    ssl_certificate_key /etc/nginx/tls/privkey.pem;

    client_max_body_size 0;
    proxy_request_buffering off;
    proxy_buffering off;
    proxy_read_timeout 900s;
    proxy_send_timeout 900s;
    proxy_http_version 1.1;

    proxy_set_header Host $host;
    # Sindri adopts this id, so its log lines match nginx's.
    proxy_set_header X-Request-Id $request_id;

    location ~ ^/(metrics|healthz|readyz)$ {
        return 404;
    }
    location / {
        proxy_pass http://sindri:8080;
    }
}
```
<!-- /proxy:nginx -->

These settings were also used by hand to push and pull a single 3.3 GB layer.

## Caddy

Caddy obtains the certificate itself, and has no body limit or timeout by default.

<!-- proxy:caddy -->
```
sindri.example.com {
    @operational path /metrics /healthz /readyz
    respond @operational 404

    reverse_proxy sindri:8080 {
        flush_interval -1
    }
}
```
<!-- /proxy:caddy -->

## Traefik

Traefik's 60-second read timeout breaks any long push, so the static configuration turns
it off:

<!-- proxy:traefik-static -->
```yaml
entryPoints:
  websecure:
    address: ":443"
    transport:
      respondingTimeouts:
        readTimeout: 0
        writeTimeout: 0
providers:
  file:
    filename: /etc/traefik/dynamic.yml
```
<!-- /proxy:traefik-static -->

The router, in `dynamic.yml`. `tls: {}` serves Traefik's default certificate, so add your
certificate or a `certResolver` to it:

<!-- proxy:traefik-dynamic -->
```yaml
http:
  routers:
    sindri:
      rule: "Host(`sindri.example.com`) && !Path(`/metrics`) && !Path(`/healthz`) && !Path(`/readyz`)"
      entryPoints: [websecure]
      service: sindri
      tls: {}
  services:
    sindri:
      loadBalancer:
        servers:
          - url: http://sindri:8080
```
<!-- /proxy:traefik-dynamic -->
