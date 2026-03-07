# Database Migrations

This directory contains SQL migration files for PostgreSQL.

## Migration Tool

We use [golang-migrate](https://github.com/golang-migrate/migrate) for schema migrations.

### Install

```bash
go install -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate@latest
```

### Usage

```bash
# Apply all pending migrations
migrate -path ./migrations -database "postgres://clawhub:clawhub@localhost:5432/clawhub?sslmode=disable" up

# Rollback last migration
migrate -path ./migrations -database "postgres://clawhub:clawhub@localhost:5432/clawhub?sslmode=disable" down 1
```

### File Naming

Migration files follow the pattern: `{version}_{description}.{up|down}.sql`

Example:
- `000001_create_agents_table.up.sql`
- `000001_create_agents_table.down.sql`

## Notes

For MVP, the schema is auto-migrated via `store.Migrate()` in Go code.
Explicit migration files will be added as the schema stabilizes.
