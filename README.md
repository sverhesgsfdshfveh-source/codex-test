# codex-test

A high-performance **Go + Gin** key batch tester for OpenAI-compatible endpoints.

## Features

- Paste keys line-by-line in a web UI
- Real-time streaming test results (NDJSON)
- Adjustable `baseUrl`, concurrency, timeout
- Pause / Resume / Stop controls
- Import keys from `.txt`
- Export successful keys to `.txt`

## Run locally

```bash
go run .
```

Default port: `18080`

Open:

- `http://127.0.0.1:18080/`
- Health check: `http://127.0.0.1:18080/healthz`

## API

### POST `/api/test-keys`
Batch test and return full result JSON.

### POST `/api/test-keys-stream`
Stream result events in NDJSON for real-time UI updates.

Request body example:

```json
{
  "baseUrl": "https://api.openai.com/v1",
  "keysText": "sk-xxx\nsk-yyy",
  "concurrency": 200,
  "timeoutSec": 8
}
```

## Notes

- For large-scale runs, tune concurrency based on upstream rate limits.
- Keep real keys out of git history and logs.
