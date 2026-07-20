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

-- Constraints
-- PRIMARY KEY enforced on INSERT: duplicate or NULL value rejected
-- FOREIGN KEY enforced on INSERT (child must match parent) and DELETE (parent can't be removed if referenced)
CREATE TABLE orders (
  id INT PRIMARY KEY,
  user_id INT,
  FOREIGN KEY (user_id) REFERENCES users(id)
);

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

```sql
-- Statistics (Phase 4)
ANALYZE players;  -- computes n_distinct, null_frac, histograms per column
                  -- output shown in execTrace; planner uses stats for next EXPLAIN

-- Transactions (Phase 5)
BEGIN;
INSERT INTO players (id, username, level, xp, class) VALUES (99, 'Neo', 50, 5000, 'Mage');
-- row invisible to other connections until COMMIT
COMMIT;
-- or: ROLLBACK;

VACUUM players;  -- reclaim dead tuples left by committed DELETEs/UPDATEs
```

## API

- `POST /query` — `{"sql": "...", "session_id": "tx-1"}` → `{columns, rows, message, tokens, ast, execTrace, session_id}`
- `GET /tables` — all tables with column schema and row counts
- `POST /seed` — drop + recreate all 10 game tables, returns `{ok, errors}`
- `POST /vacuum` — `{"table": "players"}` → `{"reclaimed": N}`

`session_id` is optional. `BEGIN` creates a new session and returns its ID. Pass it in subsequent queries for multi-statement transactions.

## Key source locations

| What | File | Notes |
|---|---|---|
| Token types | `internal/lexer/token.go` | `TokenType` constants, `Name()`, `Category()` |
| Lexer | `internal/lexer/lexer.go` | `New(sql).Tokenize()` |
| AST nodes | `internal/parser/ast.go` | All statement + expression types incl. `AnalyzeStatement`, `BeginStatement`, `CommitStatement`, `RollbackStatement`, `VacuumStatement` |
| Parser | `internal/parser/parser.go` | Recursive descent; helpers: `p.is()`, `parseOptionalWhere()`, `parseOptionalAlias()`, `parseAggParen()`, `parseIntKeyword()`, `isAggFunc()` |
| Executor | `internal/executor/executor.go` | `execSelectSingle`, `execSelectJoin`, `execSelectGroupBy`; `postProcess()` runs DISTINCT → ORDER BY → LIMIT/OFFSET; `estimateSelectivity()` uses column stats |
| Column statistics | `internal/storage/stats.go` | `ColumnStats` (NDistinct, NullFrac, Histogram, MCV), `TableStats`, `computeStats()` |
| HTTP handlers | `api/handler.go` | `Handler` struct with session store; `stmtToTrace()` / `exprToTrace()` serialize AST to JSON |
| MVCC | `internal/storage/mvcc.go` | `TxManager`, `Transaction`, `Snapshot`, `Visible()` — PG snapshot isolation rules |
| Seed data | `api/seed.go` | `seedStatements []string` — 10 game tables |
| Frontend | `cmd/server/web/index.html` | Pipeline animation, exec-order panel, Seed + Format buttons |

## AST shape

`SelectStatement` fields: `Distinct bool`, `Exprs []SelectExpr` (`ColSelectExpr` | `AggSelectExpr`), `Table`, `Alias`, `Joins []JoinClause`, `Where Expression`, `GroupBy []string`, `Having Expression`, `OrderBy []OrderByExpr`, `Limit *int64`, `Offset *int64`.

`CreateTableStatement` fields: `Table string`, `Columns []ColumnDef`, `ForeignKeys []ForeignKeyConstraint`. `ColumnDef.Primary bool` marks PRIMARY KEY columns. `ForeignKeyConstraint` holds `Column`, `RefTable`, `RefColumn` — parsed from `FOREIGN KEY (col) REFERENCES table(col)`.

Expression nodes: `BinaryExpr`, `IdentExpr`, `LiteralExpr`, `AggFuncExpr`.

## Executor dispatch

- No aggregates + no GROUP BY → `execSelectSingle` or `execSelectJoin`
- Has aggregates OR GROUP BY → `execSelectGroupBy` (handles full table as one group when no GROUP BY)
- All three paths call `postProcess()` at the end: DISTINCT dedup → `sort.SliceStable` ORDER BY → slice LIMIT/OFFSET

## Storage

All data in-memory — lost on restart. `Row` is `map[string]interface{}`. Aggregate results use synthetic rows with keys like `"COUNT(*)"`, `"SUM(xp)"` for HAVING evaluation.

`Table` struct holds `ForeignKeys []FKConstraint`. Constraint enforcement happens inside `storage.Insert` and `storage.DeleteRows`:
- **PRIMARY KEY** (INSERT): scans live tuples for duplicate PK value; rejects NULL.
- **FOREIGN KEY** (INSERT): scans parent table for matching `RefColumn` value; NULL in FK column is allowed.
- **FOREIGN KEY** (DELETE parent): scans all child tables for references to rows being deleted; rejects if found (RESTRICT).

## Statistics (Phase 4)

`ANALYZE tablename` runs `storage.computeStats()` which scans all rows and computes per-column:
- `NullFrac` — fraction of NULL values
- `NDistinct` — distinct value count (negative = all distinct, like PG)
- `AvgWidth` — average byte width
- `Histogram []interface{}` — equi-height bucket boundaries (up to 100 buckets)
- `MCV []MCVEntry` — top-10 most common values with frequencies

The executor's `estimateSelectivity()` uses these stats to replace hardcoded fractions:
- `col = val`: checks MCV, falls back to `1/n_distinct`
- `col > val` / `col < val`: binary search in histogram, returns fraction above/below
- `GROUP BY col`: uses `n_distinct` instead of `rows/5`
- `SELECT DISTINCT col`: uses `n_distinct` instead of `rows/2`

Without ANALYZE: planner falls back to PG-style defaults (0.5% for `=`, 33% for range).

## MVCC (Phase 5)

Each `Tuple` carries `Xmin uint64` (inserting txid) and `Xmax uint64` (deleting txid, 0=live).

- `INSERT`: stamps `Xmin = currentXID`
- `UPDATE`: marks old tuple `Xmax = currentXID`, inserts new version with `Xmin = currentXID` (no in-place mutate)
- `DELETE`: marks `Xmax = currentXID` (no physical removal)
- `SELECT`: filters tuples via `Visible(xmin, xmax, snapshot, currentXID, mgr)`
- `VACUUM`: physically removes tuples where `Xmax != 0` and deleting tx is committed

`xid=0` means auto-committed (legacy/single-statement mode) — always visible. Full multi-statement transactions use HTTP `session_id`. `TxManager` in `internal/storage/mvcc.go`.
