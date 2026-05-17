# http-probe

`http-probe` periodically calls configured HTTP APIs and exposes Prometheus metrics for availability checks.

It is designed to keep the HTTP prober metric surface compatible with Prometheus Blackbox Exporter where possible. Existing Grafana dashboards built for **Blackbox Exporter (HTTP prober)** can be reused directly or with minimal variable changes.

## Configuration

Configuration is intentionally simple: add the HTTP targets you want to check, then define one string that must appear in the response body.

Example `config.json.example`:

```json
{
    "listen_addr": ":9100",
    "metrics_path": "/metrics",
    "probe_interval": "60s",
    "probe_timeout": "90s",
    "targets": [
        {
            "name": "release_health",
            "method": "GET",
            "url": "https://httpbin.org/get",
            "headers": {},
            "body": {},
            "expected_body_contains": "\"origin\""
        }
    ]
}
```

Each target must have a unique `url`, which is also used as the Prometheus `target` label. `name` is optional and only used in logs.

`expected_body_contains` is the business success check: when the HTTP status code is `200` and the response body contains this string, the probe is treated as successful. If the status code is not `200`, or the response body does not contain this string, `probe_success` is set to `0`.

Header values, URL, expected string, and body support environment variable expansion with `${VAR}`. Top-level `probe_interval` and `probe_timeout` are defaults for all targets; target-level values can override them.

## Build

```powershell
go build -o bin/http-probe.exe .
```

## Run

```powershell
Copy-Item config.json.example bin/config.json
bin/http-probe.exe -config bin/config.json
```

## Docker

```powershell
docker build -f Dockerfile -t http-probe:local .
docker run --rm -p 9100:9100 `
  -e TZ=Asia/Shanghai `
  -v ${PWD}/config.json.example:/etc/http-probe/config.json:ro `
  http-probe:local
```

The bundled `config.json.example` listens on `:9100`. If `listen_addr` is omitted, the built-in default is `:9108`.

## Metrics

The main metrics use the same names and label style as Blackbox Exporter HTTP probes:

- `probe_success{target="https://proxy-a.example.com/v1/responses"}`: `1` means available, `0` means unavailable.
- `probe_duration_seconds{target="https://proxy-a.example.com/v1/responses"}`: latest full probe duration.
- `probe_http_duration_seconds{target="https://proxy-a.example.com/v1/responses",phase="resolve"}`: HTTP phase duration. Phases are `resolve`, `connect`, `tls`, `processing`, `transfer`, and `total`.
- `probe_http_status_code{target="https://proxy-a.example.com/v1/responses"}`: latest HTTP status code, `0` means no response.
- `probe_http_content_length{target="https://proxy-a.example.com/v1/responses"}`: response body length from `Content-Length`, or bytes read when the header is absent.
- `probe_http_uncompressed_body_length{target="https://proxy-a.example.com/v1/responses"}`: body bytes read by the probe.
- `probe_http_body_match{target="https://proxy-a.example.com/v1/responses"}`: latest body match result.
- `probe_failed_due_to_regex{target="https://proxy-a.example.com/v1/responses"}`: `1` means the latest failure was caused by body mismatch.
- `probe_http_ssl{target="https://proxy-a.example.com/v1/responses"}`: `1` means HTTPS/TLS was used.
- `probe_ssl_earliest_cert_expiry{target="https://proxy-a.example.com/v1/responses"}`: earliest TLS certificate expiry timestamp.
- `probe_http_version{target="https://proxy-a.example.com/v1/responses"}`: HTTP response version, for example `1.1` or `2`.
- `probe_http_redirects{target="https://proxy-a.example.com/v1/responses"}`: redirects followed by the latest probe.
- `probe_http_last_modified_timestamp_seconds{target="https://proxy-a.example.com/v1/responses"}`: parsed `Last-Modified` response header, or `0`.

Additional `http_probe_*` metrics are exported for this service's own bookkeeping:

- `http_probe_up{target="https://proxy-a.example.com/v1/responses"}`: compatibility alias for `probe_success`.
- `http_probe_last_run_timestamp_seconds{target="https://proxy-a.example.com/v1/responses"}`: latest probe attempt timestamp.
- `http_probe_last_success_timestamp_seconds{target="https://proxy-a.example.com/v1/responses"}`: latest successful probe timestamp.
- `http_probe_success_total{target="https://proxy-a.example.com/v1/responses"}`: successful probe count.
- `http_probe_failure_total{target="https://proxy-a.example.com/v1/responses",reason="bad_status"}`: failed probe count by reason.

## Grafana Compatibility

Use a Grafana dashboard made for **Blackbox Exporter (HTTP prober)** and point its Prometheus data source at the Prometheus server scraping `http-probe`.

Compatibility notes:

- Dashboard panels based on `probe_success`, `probe_duration_seconds`, `probe_http_duration_seconds`, `probe_http_status_code`, TLS certificate expiry, redirects, HTTP version, and content length should work.
- The `target` label contains the configured probe URL, matching the common Blackbox Exporter dashboard expectation.
- This exporter runs probes from its local config and exposes the latest results at `/metrics`; it does not implement Blackbox Exporter's `/probe?target=...&module=...` endpoint.
- If a dashboard filters by `job`, set the dashboard variable to the scrape job name you use below, for example `http-probe`.
