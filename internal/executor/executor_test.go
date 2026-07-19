package executor

import (
	"database/internal/lexer"
	"database/internal/parser"
	"database/internal/storage"
	"strings"
	"testing"
)

// exec parses and executes SQL against db, returning the Result or fataling.
func exec(t *testing.T, e *Executor, sql string) *Result {
	t.Helper()
	tokens := lexer.New(sql).Tokenize()
	stmt, err := parser.New(tokens).Parse()
	if err != nil {
		t.Fatalf("parse(%q): %v", sql, err)
	}
	res, err := e.Execute(stmt)
	if err != nil {
		t.Fatalf("execute(%q): %v", sql, err)
	}
	return res
}

// execErr returns the error from parsing+executing SQL.
func execErr(e *Executor, sql string) error {
	tokens := lexer.New(sql).Tokenize()
	stmt, err := parser.New(tokens).Parse()
	if err != nil {
		return err
	}
	_, err = e.Execute(stmt)
	return err
}

func newExecutor() *Executor {
	return New(storage.New())
}

func setupPlayers(t *testing.T, e *Executor) {
	t.Helper()
	exec(t, e, "CREATE TABLE players (id INT PRIMARY KEY, username TEXT, level INT, class TEXT)")
	exec(t, e, "INSERT INTO players (id, username, level, class) VALUES (1, 'Alice', 10, 'Mage')")
	exec(t, e, "INSERT INTO players (id, username, level, class) VALUES (2, 'Bob', 5, 'Warrior')")
	exec(t, e, "INSERT INTO players (id, username, level, class) VALUES (3, 'Carol', 15, 'Mage')")
	exec(t, e, "INSERT INTO players (id, username, level, class) VALUES (4, 'Dan', 8, 'Rogue')")
	exec(t, e, "INSERT INTO players (id, username, level, class) VALUES (5, 'Eve', 15, 'Warrior')")
}

// ─── CREATE / DROP ────────────────────────────────────────────────────────────

func TestCreateTable(t *testing.T) {
	e := newExecutor()
	res := exec(t, e, "CREATE TABLE t (id INT PRIMARY KEY, name TEXT)")
	if !strings.Contains(res.Message, "created") {
		t.Errorf("unexpected message: %q", res.Message)
	}
}

func TestCreateTableDuplicate(t *testing.T) {
	e := newExecutor()
	exec(t, e, "CREATE TABLE t (id INT)")
	if err := execErr(e, "CREATE TABLE t (id INT)"); err == nil {
		t.Error("expected error creating duplicate table")
	}
}

func TestDropTable(t *testing.T) {
	e := newExecutor()
	exec(t, e, "CREATE TABLE t (id INT)")
	res := exec(t, e, "DROP TABLE t")
	if !strings.Contains(res.Message, "dropped") {
		t.Errorf("unexpected message: %q", res.Message)
	}
}

func TestDropTableIfExists(t *testing.T) {
	e := newExecutor()
	res := exec(t, e, "DROP TABLE IF EXISTS nonexistent")
	if !strings.Contains(res.Message, "does not exist") {
		t.Errorf("unexpected message: %q", res.Message)
	}
}

func TestDropTableNotExists(t *testing.T) {
	e := newExecutor()
	if err := execErr(e, "DROP TABLE nonexistent"); err == nil {
		t.Error("expected error dropping non-existent table")
	}
}

// ─── INSERT ───────────────────────────────────────────────────────────────────

func TestInsert(t *testing.T) {
	e := newExecutor()
	exec(t, e, "CREATE TABLE t (id INT, name TEXT)")
	res := exec(t, e, "INSERT INTO t (id, name) VALUES (1, 'Alice')")
	if res.Message != "1 row inserted" {
		t.Errorf("message = %q", res.Message)
	}
}

func TestInsertPositional(t *testing.T) {
	e := newExecutor()
	exec(t, e, "CREATE TABLE t (id INT, name TEXT)")
	exec(t, e, "INSERT INTO t VALUES (1, 'Alice')")
	res := exec(t, e, "SELECT * FROM t")
	if len(res.Rows) != 1 {
		t.Errorf("expected 1 row, got %d", len(res.Rows))
	}
}

func TestInsertColumnMismatch(t *testing.T) {
	e := newExecutor()
	exec(t, e, "CREATE TABLE t (id INT, name TEXT)")
	if err := execErr(e, "INSERT INTO t (id) VALUES (1, 'extra')"); err == nil {
		t.Error("expected error on column/value count mismatch")
	}
}

// ─── SELECT ───────────────────────────────────────────────────────────────────

func TestSelectStar(t *testing.T) {
	e := newExecutor()
	setupPlayers(t, e)
	res := exec(t, e, "SELECT * FROM players")
	if len(res.Rows) != 5 {
		t.Errorf("expected 5 rows, got %d", len(res.Rows))
	}
}

func TestSelectColumns(t *testing.T) {
	e := newExecutor()
	setupPlayers(t, e)
	res := exec(t, e, "SELECT username, level FROM players")
	if len(res.Columns) != 2 {
		t.Errorf("expected 2 columns, got %d", len(res.Columns))
	}
	if res.Columns[0] != "username" || res.Columns[1] != "level" {
		t.Errorf("columns = %v", res.Columns)
	}
}

func TestSelectWhere(t *testing.T) {
	e := newExecutor()
	setupPlayers(t, e)
	res := exec(t, e, "SELECT * FROM players WHERE level > 10")
	if len(res.Rows) != 2 {
		t.Errorf("expected 2 rows with level > 10, got %d", len(res.Rows))
	}
}

func TestSelectWhereAnd(t *testing.T) {
	e := newExecutor()
	setupPlayers(t, e)
	res := exec(t, e, "SELECT * FROM players WHERE level > 5 AND class = 'Mage'")
	if len(res.Rows) != 2 {
		t.Errorf("expected 2 Mages with level > 5, got %d", len(res.Rows))
	}
}

func TestSelectDistinct(t *testing.T) {
	e := newExecutor()
	setupPlayers(t, e)
	res := exec(t, e, "SELECT DISTINCT class FROM players")
	if len(res.Rows) != 3 {
		t.Errorf("expected 3 distinct classes, got %d", len(res.Rows))
	}
}

func TestSelectOrderByAsc(t *testing.T) {
	e := newExecutor()
	setupPlayers(t, e)
	res := exec(t, e, "SELECT level FROM players ORDER BY level ASC")
	if len(res.Rows) != 5 {
		t.Fatalf("expected 5 rows, got %d", len(res.Rows))
	}
	prev := int64(0)
	for _, row := range res.Rows {
		v := row[0].(int64)
		if v < prev {
			t.Errorf("not sorted ASC: %d < %d", v, prev)
		}
		prev = v
	}
}

func TestSelectOrderByDesc(t *testing.T) {
	e := newExecutor()
	setupPlayers(t, e)
	res := exec(t, e, "SELECT level FROM players ORDER BY level DESC")
	if len(res.Rows) != 5 {
		t.Fatalf("expected 5 rows, got %d", len(res.Rows))
	}
	prev := int64(9999)
	for _, row := range res.Rows {
		v := row[0].(int64)
		if v > prev {
			t.Errorf("not sorted DESC: %d > %d", v, prev)
		}
		prev = v
	}
}

func TestSelectLimit(t *testing.T) {
	e := newExecutor()
	setupPlayers(t, e)
	res := exec(t, e, "SELECT * FROM players LIMIT 2")
	if len(res.Rows) != 2 {
		t.Errorf("expected 2 rows, got %d", len(res.Rows))
	}
}

func TestSelectOffset(t *testing.T) {
	e := newExecutor()
	setupPlayers(t, e)
	res := exec(t, e, "SELECT * FROM players ORDER BY id LIMIT 3 OFFSET 2")
	if len(res.Rows) != 3 {
		t.Errorf("expected 3 rows, got %d", len(res.Rows))
	}
	// with ORDER BY id and OFFSET 2, first row should be id=3
	if res.Rows[0][0].(int64) != 3 {
		t.Errorf("first row id = %v, want 3", res.Rows[0][0])
	}
}

func TestSelectLimitBeyondRowCount(t *testing.T) {
	e := newExecutor()
	setupPlayers(t, e)
	res := exec(t, e, "SELECT * FROM players LIMIT 100")
	if len(res.Rows) != 5 {
		t.Errorf("expected 5 rows (all), got %d", len(res.Rows))
	}
}

// ─── Aggregates ───────────────────────────────────────────────────────────────

func TestCount(t *testing.T) {
	e := newExecutor()
	setupPlayers(t, e)
	res := exec(t, e, "SELECT COUNT(*) FROM players")
	if len(res.Rows) != 1 || res.Rows[0][0].(int64) != 5 {
		t.Errorf("COUNT(*) = %v, want 5", res.Rows)
	}
}

func TestSum(t *testing.T) {
	e := newExecutor()
	setupPlayers(t, e)
	res := exec(t, e, "SELECT SUM(level) FROM players")
	// 10+5+15+8+15 = 53
	if len(res.Rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(res.Rows))
	}
	sum := res.Rows[0][0].(float64)
	if sum != 53 {
		t.Errorf("SUM(level) = %v, want 53", sum)
	}
}

func TestAvg(t *testing.T) {
	e := newExecutor()
	setupPlayers(t, e)
	res := exec(t, e, "SELECT AVG(level) FROM players")
	if len(res.Rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(res.Rows))
	}
	avg := res.Rows[0][0].(float64)
	// 53/5 = 10.6
	if avg != 10.6 {
		t.Errorf("AVG(level) = %v, want 10.6", avg)
	}
}

func TestMin(t *testing.T) {
	e := newExecutor()
	setupPlayers(t, e)
	res := exec(t, e, "SELECT MIN(level) FROM players")
	// MIN/MAX return the raw stored type (int64 for INT columns)
	v, _ := toFloat(res.Rows[0][0])
	if v != 5 {
		t.Errorf("MIN(level) = %v, want 5", res.Rows[0][0])
	}
}

func TestMax(t *testing.T) {
	e := newExecutor()
	setupPlayers(t, e)
	res := exec(t, e, "SELECT MAX(level) FROM players")
	v, _ := toFloat(res.Rows[0][0])
	if v != 15 {
		t.Errorf("MAX(level) = %v, want 15", res.Rows[0][0])
	}
}

func TestGroupBy(t *testing.T) {
	e := newExecutor()
	setupPlayers(t, e)
	res := exec(t, e, "SELECT class, COUNT(*) FROM players GROUP BY class ORDER BY class")
	if len(res.Rows) != 3 {
		t.Errorf("expected 3 groups, got %d", len(res.Rows))
	}
	// Mage=2, Rogue=1, Warrior=2 (alphabetical)
	counts := map[string]int64{}
	for _, row := range res.Rows {
		counts[row[0].(string)] = row[1].(int64)
	}
	if counts["Mage"] != 2 || counts["Rogue"] != 1 || counts["Warrior"] != 2 {
		t.Errorf("group counts = %v", counts)
	}
}

func TestHaving(t *testing.T) {
	e := newExecutor()
	setupPlayers(t, e)
	res := exec(t, e, "SELECT class, COUNT(*) FROM players GROUP BY class HAVING COUNT(*) > 1")
	if len(res.Rows) != 2 {
		t.Errorf("expected 2 groups with count > 1, got %d", len(res.Rows))
	}
}

// ─── UPDATE ───────────────────────────────────────────────────────────────────

func TestUpdate(t *testing.T) {
	e := newExecutor()
	setupPlayers(t, e)
	res := exec(t, e, "UPDATE players SET level = 99 WHERE username = 'Alice'")
	if !strings.Contains(res.Message, "1 row") {
		t.Errorf("message = %q", res.Message)
	}
	res = exec(t, e, "SELECT level FROM players WHERE username = 'Alice'")
	if res.Rows[0][0].(int64) != 99 {
		t.Errorf("level = %v, want 99", res.Rows[0][0])
	}
}

func TestUpdateNoWhere(t *testing.T) {
	e := newExecutor()
	setupPlayers(t, e)
	res := exec(t, e, "UPDATE players SET class = 'Paladin'")
	if !strings.Contains(res.Message, "5 row") {
		t.Errorf("expected 5 rows updated, got %q", res.Message)
	}
}

// ─── DELETE ───────────────────────────────────────────────────────────────────

func TestDelete(t *testing.T) {
	e := newExecutor()
	setupPlayers(t, e)
	exec(t, e, "DELETE FROM players WHERE level < 8")
	res := exec(t, e, "SELECT * FROM players")
	if len(res.Rows) != 4 {
		t.Errorf("expected 4 rows after delete, got %d", len(res.Rows))
	}
}

func TestDeleteAll(t *testing.T) {
	e := newExecutor()
	setupPlayers(t, e)
	exec(t, e, "DELETE FROM players")
	res := exec(t, e, "SELECT * FROM players")
	if len(res.Rows) != 0 {
		t.Errorf("expected 0 rows after delete all, got %d", len(res.Rows))
	}
}

// ─── JOIN ─────────────────────────────────────────────────────────────────────

func setupJoinTables(t *testing.T, e *Executor) {
	t.Helper()
	exec(t, e, "CREATE TABLE users (id INT PRIMARY KEY, name TEXT)")
	exec(t, e, "CREATE TABLE orders (id INT PRIMARY KEY, user_id INT, total INT)")
	exec(t, e, "INSERT INTO users (id, name) VALUES (1, 'Alice')")
	exec(t, e, "INSERT INTO users (id, name) VALUES (2, 'Bob')")
	exec(t, e, "INSERT INTO users (id, name) VALUES (3, 'Carol')")
	exec(t, e, "INSERT INTO orders (id, user_id, total) VALUES (1, 1, 100)")
	exec(t, e, "INSERT INTO orders (id, user_id, total) VALUES (2, 1, 200)")
	exec(t, e, "INSERT INTO orders (id, user_id, total) VALUES (3, 2, 50)")
}

func TestInnerJoin(t *testing.T) {
	e := newExecutor()
	setupJoinTables(t, e)
	res := exec(t, e, "SELECT u.name, o.total FROM users u JOIN orders o ON u.id = o.user_id")
	if len(res.Rows) != 3 {
		t.Errorf("expected 3 join rows, got %d", len(res.Rows))
	}
}

func TestLeftJoin(t *testing.T) {
	e := newExecutor()
	setupJoinTables(t, e)
	// Carol has no orders — left join should include her with nil total
	res := exec(t, e, "SELECT u.name, o.total FROM users u LEFT JOIN orders o ON u.id = o.user_id")
	if len(res.Rows) != 4 {
		t.Errorf("expected 4 rows (left join includes Carol), got %d", len(res.Rows))
	}
}

// ─── INDEX ────────────────────────────────────────────────────────────────────

func TestCreateDropIndex(t *testing.T) {
	e := newExecutor()
	setupPlayers(t, e)
	res := exec(t, e, "CREATE INDEX idx_level ON players (level)")
	if !strings.Contains(res.Message, "created") {
		t.Errorf("message = %q", res.Message)
	}
	res = exec(t, e, "DROP INDEX idx_level")
	if !strings.Contains(res.Message, "dropped") {
		t.Errorf("message = %q", res.Message)
	}
}

func TestIndexScanUsed(t *testing.T) {
	e := newExecutor()
	setupPlayers(t, e)
	exec(t, e, "CREATE INDEX idx_level ON players (level)")
	res := exec(t, e, "SELECT * FROM players WHERE level > 10")
	// Should use index scan — check trace
	usedIndex := false
	for _, line := range res.Trace {
		if strings.Contains(line, "Index Scan") {
			usedIndex = true
		}
	}
	if !usedIndex {
		t.Errorf("expected Index Scan in trace, got: %v", res.Trace)
	}
	if len(res.Rows) != 2 {
		t.Errorf("expected 2 rows, got %d", len(res.Rows))
	}
}

func TestDropIndexIfExists(t *testing.T) {
	e := newExecutor()
	res := exec(t, e, "DROP INDEX IF EXISTS nonexistent")
	if !strings.Contains(res.Message, "did not exist") {
		t.Errorf("message = %q", res.Message)
	}
}

// ─── ANALYZE ──────────────────────────────────────────────────────────────────

func TestAnalyze(t *testing.T) {
	e := newExecutor()
	setupPlayers(t, e)
	res := exec(t, e, "ANALYZE players")
	if !strings.Contains(res.Message, "statistics updated") {
		t.Errorf("message = %q", res.Message)
	}
	if len(res.Trace) == 0 {
		t.Error("expected analyze trace output")
	}
}

// ─── EXPLAIN ──────────────────────────────────────────────────────────────────

func TestExplain(t *testing.T) {
	e := newExecutor()
	setupPlayers(t, e)
	res := exec(t, e, "EXPLAIN SELECT * FROM players WHERE level > 5")
	if len(res.Rows) == 0 {
		t.Error("expected EXPLAIN output rows")
	}
	// should have Planning Time line
	found := false
	for _, row := range res.Rows {
		if strings.Contains(row[0].(string), "Planning Time") {
			found = true
		}
	}
	if !found {
		t.Error("EXPLAIN output missing Planning Time")
	}
}

func TestExplainAnalyze(t *testing.T) {
	e := newExecutor()
	setupPlayers(t, e)
	res := exec(t, e, "EXPLAIN ANALYZE SELECT * FROM players")
	found := false
	for _, row := range res.Rows {
		if strings.Contains(row[0].(string), "Execution Time") {
			found = true
		}
	}
	if !found {
		t.Error("EXPLAIN ANALYZE output missing Execution Time")
	}
}

func TestExplainWithIndex(t *testing.T) {
	e := newExecutor()
	setupPlayers(t, e)
	exec(t, e, "CREATE INDEX idx_level ON players (level)")
	res := exec(t, e, "EXPLAIN SELECT * FROM players WHERE level = 10")
	found := false
	for _, row := range res.Rows {
		if strings.Contains(row[0].(string), "Index Scan") {
			found = true
		}
	}
	if !found {
		t.Error("EXPLAIN should show Index Scan when index exists")
	}
}

// ─── Transactions ─────────────────────────────────────────────────────────────

func TestBeginCommit(t *testing.T) {
	e := newExecutor()
	setupPlayers(t, e)

	res := exec(t, e, "BEGIN")
	if res.Message != "BEGIN" {
		t.Errorf("BEGIN message = %q", res.Message)
	}

	exec(t, e, "INSERT INTO players (id, username, level, class) VALUES (99, 'Neo', 50, 'Mage')")

	res = exec(t, e, "COMMIT")
	if res.Message != "COMMIT" {
		t.Errorf("COMMIT message = %q", res.Message)
	}

	// verify row is visible after commit
	res = exec(t, e, "SELECT * FROM players WHERE username = 'Neo'")
	if len(res.Rows) != 1 {
		t.Errorf("expected 1 row after commit, got %d", len(res.Rows))
	}
}

func TestBeginRollback(t *testing.T) {
	e := newExecutor()
	setupPlayers(t, e)

	exec(t, e, "BEGIN")
	exec(t, e, "INSERT INTO players (id, username, level, class) VALUES (99, 'Ghost', 1, 'Mage')")
	exec(t, e, "ROLLBACK")

	// insert was rolled back — should not appear
	res := exec(t, e, "SELECT * FROM players WHERE username = 'Ghost'")
	if len(res.Rows) != 0 {
		t.Errorf("rolled back row should be invisible, got %d rows", len(res.Rows))
	}
}

func TestDoubleBeginError(t *testing.T) {
	e := newExecutor()
	exec(t, e, "CREATE TABLE t (id INT)")
	exec(t, e, "BEGIN")
	if err := execErr(e, "BEGIN"); err == nil {
		t.Error("expected error on nested BEGIN")
	}
	exec(t, e, "ROLLBACK")
}

func TestCommitWithoutBegin(t *testing.T) {
	e := newExecutor()
	exec(t, e, "CREATE TABLE t (id INT)")
	if err := execErr(e, "COMMIT"); err == nil {
		t.Error("expected error on COMMIT without BEGIN")
	}
}

func TestRollbackWithoutBegin(t *testing.T) {
	e := newExecutor()
	exec(t, e, "CREATE TABLE t (id INT)")
	if err := execErr(e, "ROLLBACK"); err == nil {
		t.Error("expected error on ROLLBACK without BEGIN")
	}
}

// ─── VACUUM ───────────────────────────────────────────────────────────────────

func TestVacuum(t *testing.T) {
	e := newExecutor()
	setupPlayers(t, e)

	exec(t, e, "BEGIN")
	exec(t, e, "DELETE FROM players WHERE level < 8")
	exec(t, e, "COMMIT")

	res := exec(t, e, "VACUUM players")
	if !strings.Contains(res.Message, "reclaimed") {
		t.Errorf("message = %q", res.Message)
	}
}

// ─── Expression evaluation edge cases ────────────────────────────────────────

func TestWhereNEQ(t *testing.T) {
	e := newExecutor()
	setupPlayers(t, e)
	res := exec(t, e, "SELECT * FROM players WHERE class != 'Mage'")
	if len(res.Rows) != 3 {
		t.Errorf("expected 3 non-Mages, got %d", len(res.Rows))
	}
}

func TestWhereLTE(t *testing.T) {
	e := newExecutor()
	setupPlayers(t, e)
	res := exec(t, e, "SELECT * FROM players WHERE level <= 8")
	if len(res.Rows) != 2 {
		t.Errorf("expected 2 rows with level <= 8, got %d", len(res.Rows))
	}
}

func TestWhereGTE(t *testing.T) {
	e := newExecutor()
	setupPlayers(t, e)
	res := exec(t, e, "SELECT * FROM players WHERE level >= 15")
	if len(res.Rows) != 2 {
		t.Errorf("expected 2 rows with level >= 15, got %d", len(res.Rows))
	}
}

func TestWhereOr(t *testing.T) {
	e := newExecutor()
	setupPlayers(t, e)
	res := exec(t, e, "SELECT * FROM players WHERE class = 'Mage' OR class = 'Rogue'")
	if len(res.Rows) != 3 {
		t.Errorf("expected 3 rows (Mages+Rogue), got %d", len(res.Rows))
	}
}

// ─── ExprString ───────────────────────────────────────────────────────────────

func TestExprString(t *testing.T) {
	expr := &parser.BinaryExpr{
		Left:  &parser.IdentExpr{Name: "age"},
		Op:    ">",
		Right: &parser.LiteralExpr{Value: int64(25)},
	}
	s := ExprString(expr)
	if s != "age > 25" {
		t.Errorf("ExprString = %q, want %q", s, "age > 25")
	}
}

func TestExprStringNull(t *testing.T) {
	if ExprString(nil) != "" {
		t.Error("ExprString(nil) should be empty string")
	}
}
