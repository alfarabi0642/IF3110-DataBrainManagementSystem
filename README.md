# DataBrain Management System

DataBrain is a server-rendered Go application for managing PostgreSQL access,
exploring schemas and data, and executing transactional SQL migrations with a
durable history.

## Run locally

Requirements: Go 1.27.3, PostgreSQL 17 or 18 (the supplied Compose fixture uses
17.11), Docker Compose, `psql`, and OpenSSL.

1. Follow [the disposable PostgreSQL setup](backend/dev/README.md).
2. From `backend/`, run `go run ./cmd/api` after applying the platform migration
   and bootstrapping the first administrator.
3. Open <http://localhost:8080>.

The server provides login/session/CSRF protection, user/group/grant and target
administration, database exploration, migration drafts, execution, recovery,
and history. It works without JavaScript.

## Checks

```bash
cd backend
go test ./...
go vet ./...
```

Without `DATABRAIN_TEST_ADMIN_URL`, PostgreSQL integration tests are skipped.
Use the fixture instructions above to run the complete suite. Configuration
examples are in [`backend/.env.example`](backend/.env.example); the application
does not load that file automatically.
