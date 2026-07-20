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
# Server port (default: 3000)
PORT=3000
```

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
