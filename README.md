# selfh.st/icons: Server Deployment

The documentation covered on this page outlines the process for deploying a container to reference and colorize [selfh.st/icons](https://selfh.st/icons) via HEX color codes.

If you're looking for icons without the hassle, visit [selfh.st/icons](https://selfh.st/icons) and use the hosted links there to begin using them immediately.

## Tags

| Name | Tag |
| :---- | :---- | 
| Latest | `latest` |
| Release | `4.0.3`, `4.0`, `4` |

## Supported Architectures

The following architectures are supported by this image. Docker will automatically detect the appropriate value for your machine - no special tags are required.

| Architecture | 
| :----: | 
| x86-64 | 
| arm64 | 
| arm/v7 | 

## Icon Files

This image is configured to reference remote icon files by default. For local asset hosting, consider obtaining the [selfhst/icons](https://github.com/selfhst/icons) collection using one of the following options:

* rsync
* Cloning the repository to a local folder
* External tools like [git-sync](https://github.com/AkashRajpurohit/git-sync)

# Deployment

## Docker

By default, no environment variables or volume mounts are required to deploy the container. Once deployed, follow the URL conventions under [Building Links](https://github.com/selfhst/icons-server#building-links) to begin referencing and colorizing icons.

```yaml
services:
  selfhst-icons:
    image: ghcr.io/selfhst/icons:latest
    container_name: selfhst-icons
    restart: unless-stopped
    ports:
      - 4050:4050
```

## Customization

The following optional variables and volume mounts enable additional customization, including serving icons from local files.

### Environment Variables

| Variable | Default | Description |
| :---- | :----: | :---- |
| `PORT` | `4050` | Internal port for communicating with the container |
| `PRIMARY_COLOR` | `none` | HEX color variable for applying and updating a single color across icons|
| `ICON_SOURCE` | `remote` | <ul><li>`remote`: Serve icons from a remote source (defaults to [selfhst/icons](https://github.com/selfhst/icons), override with `REMOTE_URL`)</li><li>`local`: Serve icons from local files with mounted volumes</li><li>`hybrid`: Serve icons from local files when available, fall back to remote when not</li></ul> |
| `REMOTE_URL` | `https://cdn.jsdelivr.net/gh/selfhst/icons@main` | Remote URL for icons when `remote` or `hybrid` source is selected |
| `LOG_LEVEL` | `info` | <ul><li>`error`: 404s, path traversals, invalid extensions, read failures</li><li>`info`: All errors, plus non-cache fetches and custom icons</li><li>`debug`: All of the above, plus cache hits</li></ul> |
| `CACHE_TTL` | `3600` | How long icons are cached in seconds |
| `CACHE_SIZE` | `500` | Maximum number of icons stored in cache |
| `REMOTE_TIMEOUT` | `10` | Seconds to wait for a remote request before timing out |
| `CORS_ALLOWED_ORIGINS` | `*` | Allowed origin(s) for cross-origin requests (must include `http://` or `https://`) |

### Docker (Extended)

```yaml
services:

  selfhst-icons:
    image: ghcr.io/selfhst/icons:latest
    container_name: selfhst-icons
    restart: unless-stopped
    ports:
      - 4050:4050
      
    environment:
      PORT: 4050
      PRIMARY_COLOR: 0f60d9
      ICON_SOURCE: remote
      REMOTE_URL: https://cdn.jsdelivr.net/gh/selfhst/icons@main
      LOG_LEVEL: info
      CACHE_TTL: 3600
      CACHE_SIZE: 500
      REMOTE_TIMEOUT: 10
      CORS_ALLOWED_ORIGINS: "*"
     
    # Only necessary if ICON_SOURCE is set to 'local' or 'hybrid', or if mounting custom icons alongside a 'remote' deployment
    volumes:
      - /local/path/to/svg:/app/icons/svg:ro  # Required for local/hybrid custom colors, optional if not using custom colors
      - /local/path/to/png:/app/icons/png:ro  # Optional
      - /local/path/to/webp:/app/icons/webp:ro  # Optional
      - /local/path/to/avif:/app/icons/avif:ro  # Optional
      - /local/path/to/ico:/app/icons/ico:ro  # Optional
      - /local/path/to/custom:/app/icons/custom:ro  # Optional, can be used with local or remote deployments
```

## Reverse Proxy

While out of scope for this documentation, many deployments will require HTTPS. A sample Caddyfile has been provided as a reference for proxying the container.

```
icons.selfh.st {
    reverse_proxy selfhst-icons:4050
}
```

## Building Links

After deployment, icons can be referenced with the following URL conventions:

<table>
  <thead>
    <tr>
      <th align="left">Icon</th>
      <th align="left">Valid URLs</th>
      <th align="left">Notes</th>
    </tr>
  </thead>
  <tbody>
    <tr>
      <td rowspan="3">Custom Colors</td>
      <td><code>https://example.com/plex/0f60d9</code></td>
      <td></td>
    </tr>
    <tr>
      <td><code>https://example.com/plex?color=0f60d9</code></td>
      <td></td>
    </tr>
    <tr>
      <td><code>https://example.com/plex.svg?color=0f60d9</code></td>
      <td>Use this if your application requires icon extensions in the URL</td>
    </tr>
    <tr>
      <td rowspan="3">Primary Color</td>
      <td><code>https://example.com/plex/primary</code></td>
      <td></td>
    </tr>
    <tr>
      <td><code>https://example.com/plex?color=primary</code></td>
      <td></td>
    </tr>
    <tr>
      <td><code>https://example.com/plex.svg?color=primary</code></td>
      <td>Use this if your application requires icon extensions in the URL</td>
    </tr>
    <tr>
      <td rowspan="7">Standard Icons</td>
      <td><code>https://example.com/plex</code></td>
      <td>No extension, defaults to WebP</td>
    </tr>
    <tr>
      <td><code>https://example.com/plex.svg</code></td>
      <td></td>
    </tr>
    <tr>
      <td><code>https://example.com/plex.png</code></td>
      <td></td>
    </tr>
    <tr>
      <td><code>https://example.com/plex.webp</code></td>
      <td></td>
    </tr>
    <tr>
      <td><code>https://example.com/plex.avif</code></td>
      <td></td>
    </tr>
    <tr>
      <td><code>https://example.com/plex.ico</code></td>
      <td></td>
    </tr>
    <tr>
      <td><code>https://example.com/plex.gif</code></td>
      <td>Unsupported extension, will fall back to WebP</td>
    </tr>
    <tr>
      <td>Custom Icon</td>
      <td><code>https://example.com/custom/plex.png</code></td>
      <td>Custom icons require a volume mount, must contain an extension in the URL, cannot be colorized, and are not limited to the extensions above</td>
    </tr>
  </tbody>
</table>

## License

The code in this repo is licensed MIT. The icons are licensed CC BY 4.0, per the [source repo](https://github.com/selfhst/icons).

## AI Disclaimer

This project leverages AI to assist with development. Code contributions generated or modified with the help of AI tools are reviewed and tested by maintainers before release.
