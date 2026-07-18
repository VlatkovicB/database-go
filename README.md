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
POST /query     {"sql": "..."}  →  {columns, rows, message, tokens, ast, execTrace}
GET  /tables                    →  {tables: [{name, columns, rowCount}]}
POST /seed                      →  {ok, errors}   — repopulates all 10 game tables
```

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
