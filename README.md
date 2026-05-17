# http-probe

`http-probe` periodically calls configured HTTP APIs and exposes Prometheus metrics for availability checks.

## Build

```powershell
go build -o bin/http-probe.exe .
```

## Run

```powershell
Copy-Item config.json.example bin/config.json
$env:OPENAI_API_KEY="sk-..."
bin/http-probe.exe -config bin/config.json
```

## Docker

```powershell
docker build -f Dockerfile -t http-probe:local .
docker run --rm -p 9108:9108 `
  -e OPENAI_API_KEY="sk-..." `
  -e TZ=Asia/Shanghai `
  -v ${PWD}/config.json.example:/etc/http-probe/config.json:ro `
  http-probe:local
```

Build and push the versioned image on Windows:

```powershell
.\build_and_push_docker.bat
```

## Config

Each target must have a unique `url`, used as the Prometheus `target` label. `name` is optional and only used in logs. Header values, URL, expected string, and body support environment variable expansion with `${VAR}`.

Top-level `probe_interval` and `probe_timeout` are defaults for all targets. Each target can set its own `probe_interval` and `probe_timeout`; target values override the top-level defaults. If neither top-level nor target-level values are set, the built-in defaults are `30s` and `20s`.

Success means:

1. HTTP status code is `200`.
2. Response body contains `expected_body_contains`.

## Metrics

- `probe_success{target="https://proxy-a.example.com/v1/responses"}`: `1` means available, `0` means unavailable.
- `http_probe_up{target="https://proxy-a.example.com/v1/responses"}`: compatibility alias for `probe_success`.
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
- `http_probe_last_run_timestamp_seconds{target="https://proxy-a.example.com/v1/responses"}`: latest probe attempt timestamp.
- `http_probe_last_success_timestamp_seconds{target="https://proxy-a.example.com/v1/responses"}`: latest successful probe timestamp.
- `http_probe_success_total{target="https://proxy-a.example.com/v1/responses"}`: successful probe count.
- `http_probe_failure_total{target="https://proxy-a.example.com/v1/responses",reason="bad_status"}`: failed probe count by reason.

## Prometheus

```yaml
scrape_configs:
  - job_name: http-probe
    static_configs:
      - targets:
          - localhost:9108
```

## Alert Rule

```yaml
groups:
  - name: http-probe
    rules:
      - alert: OpenAIProxyUnavailable
        expr: probe_success == 0
        for: 2m
        labels:
          severity: critical
        annotations:
          summary: "OpenAI proxy unavailable: {{ $labels.target }}"
          description: "{{ $labels.target }} has failed HTTP probe checks for more than 2 minutes."
```
