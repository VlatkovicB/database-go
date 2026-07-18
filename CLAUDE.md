# MiniDB

PostgreSQL-subset database engine built in Go for learning how query execution works.

## Run

```bash
go run ./cmd/server/
# open http://localhost:8080
```

## Architecture

Query pipeline: SQL string → Lexer → Parser → Executor → Result

```
internal/
  lexer/      # Tokenizes SQL string into typed tokens
  parser/     # Recursive descent parser → AST
  storage/    # In-memory tables (sync.RWMutex protected)
  executor/   # Walks AST, runs against storage, evaluates WHERE
api/          # HTTP handlers: POST /query, GET /tables
cmd/server/   # Entry point + embedded frontend (web/index.html)
```

## Supported SQL

```sql
CREATE TABLE users (id INT PRIMARY KEY, name TEXT, age INT);
INSERT INTO users (id, name, age) VALUES (1, 'Alice', 30);
INSERT INTO users VALUES (2, 'Bob', 24);
SELECT * FROM users WHERE age > 25 AND name != 'Eve';
UPDATE users SET age = 31 WHERE id = 1;
DELETE FROM users WHERE age < 25;
DROP TABLE users;
```

## API

- `POST /query` — `{"sql": "..."}` → `{columns, rows, message, tokens, ast, execTrace}`
- `GET /tables` — returns all tables with column schema and row counts

## Storage

All data is in-memory — lost on restart. `Row` is `map[string]interface{}`. Types: `INT`, `TEXT`, `BOOLEAN`, `FLOAT`.

## Frontend

Single-page app served from `cmd/server/web/index.html`. Shows execution pipeline (Lexer → Parser → Executor) with adjustable animation speed.
