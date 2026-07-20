# MiniDB

A PostgreSQL-subset database engine built from scratch in Go — for learning how query execution actually works.

No external database libraries. No ORMs. Just lexer → parser → executor → results.

## What it does

MiniDB parses and executes SQL against an in-memory store, then shows you every step of the pipeline in a browser UI:

- **Lexer** — tokenizes your SQL into typed tokens
- **Parser** — builds an AST via recursive descent
- **Executor** — walks the AST and runs it against storage
- **Execution order** — visualizes the logical SQL clause order (FROM → JOIN → WHERE → GROUP BY → HAVING → SELECT → DISTINCT → ORDER BY → LIMIT → OFFSET)

## Run

```bash
go run ./cmd/server/
# Open http://localhost:8080
```

Click **⚡ Seed Game DB** in the header to load 10 pre-built game tables (~277 rows) to query against.

## Supported SQL

```sql
-- Tables
CREATE TABLE players (id INT PRIMARY KEY, username TEXT, level INT, class TEXT, xp INT);
DROP TABLE players;

-- Foreign keys
CREATE TABLE guilds (id INT PRIMARY KEY, name TEXT);
CREATE TABLE members (
  id INT PRIMARY KEY,
  guild_id INT,
  FOREIGN KEY (guild_id) REFERENCES guilds(id)
);
-- INSERT INTO members ... fails if guild_id has no match in guilds
-- DELETE FROM guilds ... fails if any members row references that guild

-- Write
INSERT INTO players (id, username, level, class, xp) VALUES (1, 'Alice', 42, 'Mage', 9200);
UPDATE players SET level = 43 WHERE id = 1;
DELETE FROM players WHERE level < 10;

-- Basic select
SELECT * FROM players WHERE level > 30 AND class != 'Rogue';
SELECT DISTINCT class FROM players;

-- Joins
SELECT p.username, g.name
FROM players p
JOIN guilds g ON p.guild_id = g.id;

SELECT p.username, g.name
FROM players p
LEFT JOIN guilds g ON p.guild_id = g.id;

-- Aggregates
SELECT class, COUNT(*), AVG(level), MAX(xp)
FROM players
GROUP BY class;

SELECT class, COUNT(*)
FROM players
GROUP BY class
HAVING COUNT(*) > 3;

-- Sorting and pagination
SELECT username, level
FROM players
ORDER BY level DESC
LIMIT 10 OFFSET 5;

-- Combined
SELECT class, COUNT(*), AVG(level)
FROM players
WHERE xp > 1000
GROUP BY class
HAVING COUNT(*) > 2
ORDER BY COUNT(*) DESC
LIMIT 5;

-- Statistics (like PostgreSQL ANALYZE)
ANALYZE players;
-- Shows n_distinct, null_frac, histogram bounds, most common values per column.
-- After ANALYZE, EXPLAIN row estimates use real statistics instead of fixed fractions.

-- Transactions (like PostgreSQL MVCC)
BEGIN;
INSERT INTO players (id, username, level, xp, class) VALUES (99, 'Neo', 50, 5000, 'Mage');
-- Row is invisible to other connections until you COMMIT
COMMIT;
-- or: ROLLBACK;  -- discards all changes in the transaction

VACUUM players;
-- Reclaims dead tuples left behind by committed DELETEs and UPDATEs.
```

### Column types

| Type | Example |
|---|---|
| `INT` | `42` |
| `TEXT` | `'Alice'` |
| `FLOAT` | `3.14` |
| `BOOLEAN` | `TRUE` / `FALSE` |

### Comparison operators

`=` `!=` `<` `>` `<=` `>=` — combined with `AND` / `OR`

## API

```
POST /query     {"sql": "...", "session_id": "tx-1"}  →  {columns, rows, message, tokens, ast, execTrace, session_id}
GET  /tables                                           →  {tables: [{name, columns, rowCount}]}
POST /seed                                             →  {ok, errors}   — repopulates all 10 game tables
POST /vacuum    {"table": "players"}                   →  {reclaimed: N}
```

`session_id` is optional. `BEGIN` creates a session and returns its ID. Pass that ID in subsequent queries to run them in the same transaction. `COMMIT` or `ROLLBACK` closes the session.

## Project structure

```
internal/
  lexer/       token types, tokenizer
  parser/      recursive descent parser, AST node types
  storage/     in-memory tables with sync.RWMutex
  executor/    AST walker, WHERE/GROUP BY/ORDER BY/LIMIT/OFFSET evaluation
api/
  handler.go   HTTP handlers, AST → JSON serialization
  seed.go      10 game tables with seed data
cmd/server/
  main.go          HTTP server entry point
  web/index.html   single-page frontend, embedded in binary
```

## Architecture notes

All data is in-memory and lost on restart. `Row` is `map[string]interface{}`.

SELECT execution has three paths:
- Plain SELECT (no joins, no aggregates) → `execSelectSingle`
- JOIN → `execSelectJoin`
- Any aggregate or GROUP BY → `execSelectGroupBy`

All three paths finish through `postProcess`: DISTINCT dedup → ORDER BY sort → LIMIT/OFFSET slice.

### Constraints

**PRIMARY KEY** — enforced on INSERT. Duplicate key values are rejected with an error. NULL is also rejected.

**FOREIGN KEY** — declared as a table-level constraint. Two rules are enforced:
- INSERT into child table: referenced value must exist in the parent table.
- DELETE from parent table: rejected (RESTRICT) if any child row references the deleted value.

### MVCC (Phase 5)

Each stored tuple has `Xmin` (inserting transaction ID) and `Xmax` (deleting transaction ID, 0 = live):

- **INSERT** stamps `Xmin = currentXID`
- **UPDATE** marks old tuple `Xmax = currentXID`, inserts new version with `Xmin = currentXID` — no in-place mutation
- **DELETE** marks `Xmax = currentXID` — tuple stays on the page until VACUUM
- **SELECT** filters tuples via snapshot visibility: reads committed state as of `BEGIN` time (snapshot isolation)
- **VACUUM** physically removes tuples where `Xmax != 0` and the deleting transaction is committed

Single-statement queries auto-commit (`xid=0`, always visible). Multi-statement transactions use HTTP `session_id`.

### Statistics engine (Phase 4)

`ANALYZE table` computes per-column statistics stored in `Table.Stats`:
- **`n_distinct`** — number of distinct values (negative = all rows are distinct, PG convention)
- **`null_frac`** — fraction of NULL values
- **`histogram`** — equi-height bucket boundaries (up to 100 buckets, like PG's `pg_stats.histogram_bounds`)
- **`most_common_vals`** — top-10 values by frequency

The planner's `estimateSelectivity()` uses these to produce accurate row estimates in EXPLAIN:
- `=` predicate: checks most-common-values list, falls back to `1/n_distinct`
- range predicate (`>`, `<`, `>=`, `<=`): binary searches histogram to find fraction above/below threshold
- `GROUP BY`: uses column's `n_distinct` for aggregate output row estimate
- `DISTINCT`: uses `n_distinct` of projected column

Without ANALYZE, the planner falls back to PostgreSQL-style defaults (0.5% selectivity for equality, 33% for range).
