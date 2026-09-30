# Preview Proxy Server (Go)

This is a reverse proxy that routes preview traffic to the correct sandbox,
provider-agnostically (Daytona and e2b). It parses an opaque preview ID from
the request's hostname, resolves it to an upstream URL and auth token via
[mas](https://github.com/brainbaselabs/brainbase-mas)'s
`POST /internal/preview/resolve` endpoint, and injects the returned token on
the way upstream.

## Features

- **Dynamic Routing**: Parses the preview ID from the request's hostname
  (`{previewId}.{PREVIEW_BASE_DOMAIN}`)
- **Two Upstreams, One Wildcard**: A hyphen-free label is a sandbox preview id
  and is resolved through mas as above. A hyphenated label names a published
  Brainbase Interfaces dashboard and is forwarded to the Interfaces gateway
  instead. See [Interfaces dashboards](#interfaces-dashboards)
- **Provider-Agnostic Auth**: Resolves the upstream URL and auth token/header
  via mas, then injects whatever header mas returns (e.g.
  `X-Daytona-Preview-Token` or `e2b-traffic-access-token`)
- **Smart Caching**: In-memory caching (2 minutes) of resolved previews to
  reduce latency and load on mas
- **Streaming-Safe**: No write deadline on the server, so long downloads, SSE,
  and slow-loading apps are not cut off mid-response
- **Production-Ready**: Graceful shutdown and proper error handling
- **Input Validation**: Validation of preview IDs with proper error responses
- **Health Checks**: Built-in health check endpoint at `/health`
- **Simple Configuration**: Minimal environment variables required

## Configuration

The proxy is configured using environment variables. You can place these in a
`.env` file in the project root.

### Required Environment Variables

```bash
# The base URL of the brainbase-mas service (used to resolve opaque preview
# ids to an upstream URL + auth token, provider-agnostically for Daytona and
# e2b)
MAS_BASE_URL=https://mas.brainbaselabs.com

# Shared secret sent as X-Internal-Secret when calling mas's
# POST /internal/preview/resolve endpoint
PREVIEW_RESOLVE_SECRET=your-secret

# The base domain previews are served under, e.g. requests to
# {previewId}.<this> are resolved and proxied
PREVIEW_BASE_DOMAIN=brainbaselabs.space
```

### Optional Environment Variables

```bash
# Where dashboard hosts are forwarded (see "Interfaces dashboards" below).
# A bare origin, no path. Unset means this deployment serves no dashboards.
INTERFACES_GATEWAY_URL=https://interfaces-gateway.internal

# Server port (default: 3000)
PORT=3000
```

## Interfaces dashboards

`*.{PREVIEW_BASE_DOMAIN}` carries two unrelated things, and the leftmost label
says which.

| label | example | upstream |
| --- | --- | --- |
| no hyphen | `b7f3a9c1.<domain>` | a sandbox preview, resolved through mas |
| hyphenated | `ops-console.<domain>` | the Interfaces gateway |
| hyphenated, `app--` prefix | `app--ops-console.<domain>` | the Interfaces gateway |

The two sets cannot overlap. A preview id is validated against
`^[a-zA-Z0-9]+$`, which has no hyphen, and brainbase-mas refuses to allocate a
dashboard slug without one (`check_slug` in `src/interfaces/slugs.py`, for this
exact reason). A dashboard has two hosts because the gateway's own page and the
generated dashboard have to be different origins; they are siblings rather than
nested because `*.<domain>` is a wildcard certificate and a wildcard matches
exactly one label.

Recognition is the slug grammar, not the presence of a hyphen: lowercase
alphanumerics in hyphen-separated groups, at least two groups, no leading,
trailing or doubled hyphen, 3 to 58 characters. So `_acme-challenge` and a
punycode `xn--…` label are not dashboards and keep answering as they always
have. A hyphenated *platform* host added under this domain in future would be
captured, and has to be excluded here explicitly.

Nothing is resolved on this side. The gateway already resolves every host it
serves through mas and refuses one mas does not answer for, so an unknown
dashboard host reaches the gateway and gets the gateway's own 404. A resolution
here would be a second copy of that decision and a third round trip.

The one thing the hop has to carry is the host the viewer asked for. `Host` is
rewritten to the gateway, as it must be for the request to route there at all,
so the viewer's host travels in `X-Forwarded-Host` - the header the gateway
already reads, and the one thing it decides its role, its cookie scope and which
interface is being asked for from. Because it decides access from that header,
the value is **rebuilt** from the validated label plus the configured base
domain rather than copied from the request, and `X-Forwarded-Host`,
`X-Original-Host`, `X-Host` and `Forwarded` are all cleared first. A client that
sends any of them cannot change which dashboard its request is checked against.

The gateway reads that header under a name of its own
(`INTERFACES_FORWARDED_HOST_HEADER`, default `x-forwarded-host`). Renaming it
there without changing this proxy fails closed - the gateway falls back to
`Host`, which is its own name, and refuses - but it also stops this proxy from
overwriting what a client sent, so the two settings have to move together.

### Setup Instructions

1. **Create the `.env` file:**

   ```sh
   cp .env.example .env
   ```

2. **Edit the `.env` file with your credentials and desired configuration**

## Running the Proxy

To run the proxy server, execute the following command in the project root:

```sh
go run main.go
```

The server will start on the port specified in your `.env` file (or default
to port 3000).

## Deployment with Docker

Using Docker is the recommended way to deploy the proxy as it creates a
portable, consistent, and isolated environment. Environment variables are
injected at runtime for security.

### 1. Build the Docker Image

From the project root, run the following command to build the Docker image.
This will create a lightweight, production-ready image named
`daytona-proxy`.

```sh
docker build -t daytona-proxy .
```

### 2. Run the Docker Container

Run the container with environment variables. This will start the proxy in
the background, map the internal port `3000` to the host's port `3000`, and
automatically restart it if it fails.

```sh
docker run -d --restart always -p 3000:3000 \
  -e MAS_BASE_URL="https://mas.brainbaselabs.com" \
  -e PREVIEW_RESOLVE_SECRET="your-secret" \
  -e PREVIEW_BASE_DOMAIN="brainbaselabs.space" \
  -e PORT="3000" \
  --name daytona-proxy daytona-proxy
```

Alternatively, you can use an environment file:

```sh
docker run -d --restart always -p 3000:3000 \
  --env-file .env \
  --name daytona-proxy daytona-proxy
```

### Managing the Container

```sh
# View logs
docker logs -f daytona-proxy

# Stop the container
docker stop daytona-proxy

# Start the container
docker start daytona-proxy

# Remove the container
docker rm daytona-proxy
```
