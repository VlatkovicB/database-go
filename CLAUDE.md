# MiniDB

PostgreSQL-subset database engine built in Go for learning how query execution works.

> **Keep in sync:** When SQL features, API endpoints, or architecture changes, update both this file (`CLAUDE.md`) and `README.md`.

## Run

```bash
go run ./cmd/server/
# open http://localhost:8080
```

## Architecture

Query pipeline: SQL string → Lexer → Parser → Executor → Result

```
internal/
  lexer/      # Tokenizes SQL string into typed tokens (token.go, lexer.go)
  parser/     # Recursive descent parser → AST (parser.go, ast.go)
  storage/    # In-memory tables, sync.RWMutex protected
  executor/   # Walks AST, evaluates WHERE/GROUP BY/ORDER BY/LIMIT/OFFSET
api/
  handler.go  # POST /query, GET /tables HTTP handlers
  seed.go     # POST /seed — repopulates game DB (10 tables, ~277 rows)
cmd/server/
  main.go     # Entry point, registers routes
  web/index.html  # Embedded single-page frontend
```

## Supported SQL

```sql
-- DDL
CREATE TABLE users (id INT PRIMARY KEY, name TEXT, age INT, active BOOLEAN);
DROP TABLE users;

-- DML
INSERT INTO users (id, name, age) VALUES (1, 'Alice', 30);
INSERT INTO users VALUES (2, 'Bob', 24);
UPDATE users SET age = 31 WHERE id = 1;
DELETE FROM users WHERE age < 25;

-- SELECT
SELECT * FROM users WHERE age > 25 AND name != 'Eve';
SELECT DISTINCT class FROM players ORDER BY class;
SELECT u.name, o.total FROM users u JOIN orders o ON u.id = o.user_id;
SELECT u.name, o.total FROM users u LEFT JOIN orders o ON u.id = o.user_id;

-- Aggregates + GROUP BY
SELECT class, COUNT(*), AVG(level) FROM players GROUP BY class;
SELECT class, COUNT(*) FROM players GROUP BY class HAVING COUNT(*) > 2;

-- ORDER BY / LIMIT / OFFSET
SELECT username, level FROM players ORDER BY level DESC LIMIT 10 OFFSET 5;
SELECT class, COUNT(*) FROM players GROUP BY class ORDER BY COUNT(*) DESC;
```

Column types: `INT`, `TEXT`, `BOOLEAN`, `FLOAT`.

## API

- `POST /query` — `{"sql": "..."}` → `{columns, rows, message, tokens, ast, execTrace}`
- `GET /tables` — all tables with column schema and row counts
- `POST /seed` — drop + recreate all 10 game tables, returns `{ok, errors}`

## Key source locations

| What | File | Notes |
|---|---|---|
| Token types | `internal/lexer/token.go` | `TokenType` constants, `Name()`, `Category()` |
| Lexer | `internal/lexer/lexer.go` | `New(sql).Tokenize()` |
| AST nodes | `internal/parser/ast.go` | All statement + expression types |
| Parser | `internal/parser/parser.go` | Recursive descent; helpers: `p.is()`, `parseOptionalWhere()`, `parseOptionalAlias()`, `parseAggParen()`, `parseIntKeyword()`, `isAggFunc()` |
| Executor | `internal/executor/executor.go` | `execSelectSingle`, `execSelectJoin`, `execSelectGroupBy`; `postProcess()` runs DISTINCT → ORDER BY → LIMIT/OFFSET |
| HTTP handlers | `api/handler.go` | `stmtToTrace()` / `exprToTrace()` serialize AST to JSON |
| Seed data | `api/seed.go` | `seedStatements []string` — 10 game tables |
| Frontend | `cmd/server/web/index.html` | Pipeline animation, exec-order panel, Seed + Format buttons |

## AST shape

`SelectStatement` fields: `Distinct bool`, `Exprs []SelectExpr` (`ColSelectExpr` | `AggSelectExpr`), `Table`, `Alias`, `Joins []JoinClause`, `Where Expression`, `GroupBy []string`, `Having Expression`, `OrderBy []OrderByExpr`, `Limit *int64`, `Offset *int64`.

Expression nodes: `BinaryExpr`, `IdentExpr`, `LiteralExpr`, `AggFuncExpr`.

## Executor dispatch

- No aggregates + no GROUP BY → `execSelectSingle` or `execSelectJoin`
- Has aggregates OR GROUP BY → `execSelectGroupBy` (handles full table as one group when no GROUP BY)
- All three paths call `postProcess()` at the end: DISTINCT dedup → `sort.SliceStable` ORDER BY → slice LIMIT/OFFSET

## Storage

All data in-memory — lost on restart. `Row` is `map[string]interface{}`. Aggregate results use synthetic rows with keys like `"COUNT(*)"`, `"SUM(xp)"` for HAVING evaluation.
