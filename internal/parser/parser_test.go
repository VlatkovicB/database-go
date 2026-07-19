package parser

import (
	"database/internal/lexer"
	"testing"
)

func parse(t *testing.T, sql string) Statement {
	t.Helper()
	tokens := lexer.New(sql).Tokenize()
	stmt, err := New(tokens).Parse()
	if err != nil {
		t.Fatalf("parse(%q): %v", sql, err)
	}
	return stmt
}

func parseErr(sql string) error {
	tokens := lexer.New(sql).Tokenize()
	_, err := New(tokens).Parse()
	return err
}

// ─── SELECT ───────────────────────────────────────────────────────────────────

func TestSelectStar(t *testing.T) {
	stmt := parse(t, "SELECT * FROM users")
	sel, ok := stmt.(*SelectStatement)
	if !ok {
		t.Fatal("expected *SelectStatement")
	}
	if sel.Table != "users" {
		t.Errorf("Table = %q, want users", sel.Table)
	}
	if sel.Exprs != nil {
		t.Errorf("SELECT * should produce nil Exprs, got %v", sel.Exprs)
	}
}

func TestSelectColumns(t *testing.T) {
	stmt := parse(t, "SELECT id, name FROM users")
	sel := stmt.(*SelectStatement)
	if len(sel.Exprs) != 2 {
		t.Fatalf("expected 2 exprs, got %d", len(sel.Exprs))
	}
	if sel.Exprs[0].(*ColSelectExpr).Col != "id" {
		t.Errorf("first col = %q, want id", sel.Exprs[0].(*ColSelectExpr).Col)
	}
	if sel.Exprs[1].(*ColSelectExpr).Col != "name" {
		t.Errorf("second col = %q, want name", sel.Exprs[1].(*ColSelectExpr).Col)
	}
}

func TestSelectWhere(t *testing.T) {
	stmt := parse(t, "SELECT * FROM users WHERE age > 25")
	sel := stmt.(*SelectStatement)
	bin, ok := sel.Where.(*BinaryExpr)
	if !ok {
		t.Fatal("WHERE should be BinaryExpr")
	}
	if bin.Op != ">" {
		t.Errorf("Op = %q, want >", bin.Op)
	}
	if bin.Left.(*IdentExpr).Name != "age" {
		t.Errorf("left = %q, want age", bin.Left.(*IdentExpr).Name)
	}
	if bin.Right.(*LiteralExpr).Value.(int64) != 25 {
		t.Errorf("right = %v, want 25", bin.Right.(*LiteralExpr).Value)
	}
}

func TestSelectWhereAnd(t *testing.T) {
	stmt := parse(t, "SELECT * FROM users WHERE age > 18 AND active = TRUE")
	sel := stmt.(*SelectStatement)
	bin := sel.Where.(*BinaryExpr)
	if bin.Op != "AND" {
		t.Errorf("root op = %q, want AND", bin.Op)
	}
}

func TestSelectGroupBy(t *testing.T) {
	stmt := parse(t, "SELECT class, COUNT(*) FROM players GROUP BY class")
	sel := stmt.(*SelectStatement)
	if len(sel.GroupBy) != 1 || sel.GroupBy[0] != "class" {
		t.Errorf("GroupBy = %v, want [class]", sel.GroupBy)
	}
	if len(sel.Exprs) != 2 {
		t.Fatalf("expected 2 exprs, got %d", len(sel.Exprs))
	}
	agg, ok := sel.Exprs[1].(*AggSelectExpr)
	if !ok {
		t.Fatal("second expr should be AggSelectExpr")
	}
	if agg.Func != "COUNT" || agg.Arg != "*" {
		t.Errorf("agg = {%s %s}, want {COUNT *}", agg.Func, agg.Arg)
	}
}

func TestSelectHaving(t *testing.T) {
	stmt := parse(t, "SELECT class, COUNT(*) FROM players GROUP BY class HAVING COUNT(*) > 2")
	sel := stmt.(*SelectStatement)
	if sel.Having == nil {
		t.Fatal("Having should not be nil")
	}
}

func TestSelectOrderBy(t *testing.T) {
	stmt := parse(t, "SELECT * FROM players ORDER BY level DESC, name ASC")
	sel := stmt.(*SelectStatement)
	if len(sel.OrderBy) != 2 {
		t.Fatalf("expected 2 OrderBy, got %d", len(sel.OrderBy))
	}
	if sel.OrderBy[0].Col != "level" || !sel.OrderBy[0].Desc {
		t.Errorf("OrderBy[0] = %+v, want {level DESC}", sel.OrderBy[0])
	}
	if sel.OrderBy[1].Col != "name" || sel.OrderBy[1].Desc {
		t.Errorf("OrderBy[1] = %+v, want {name ASC}", sel.OrderBy[1])
	}
}

func TestSelectLimitOffset(t *testing.T) {
	stmt := parse(t, "SELECT * FROM players LIMIT 10 OFFSET 5")
	sel := stmt.(*SelectStatement)
	if sel.Limit == nil || *sel.Limit != 10 {
		t.Errorf("Limit = %v, want 10", sel.Limit)
	}
	if sel.Offset == nil || *sel.Offset != 5 {
		t.Errorf("Offset = %v, want 5", sel.Offset)
	}
}

func TestSelectDistinct(t *testing.T) {
	stmt := parse(t, "SELECT DISTINCT class FROM players")
	sel := stmt.(*SelectStatement)
	if !sel.Distinct {
		t.Error("Distinct should be true")
	}
}

func TestSelectAlias(t *testing.T) {
	stmt := parse(t, "SELECT * FROM users u")
	sel := stmt.(*SelectStatement)
	if sel.Alias != "u" {
		t.Errorf("Alias = %q, want u", sel.Alias)
	}
}

func TestSelectInnerJoin(t *testing.T) {
	stmt := parse(t, "SELECT u.name, o.total FROM users u JOIN orders o ON u.id = o.user_id")
	sel := stmt.(*SelectStatement)
	if len(sel.Joins) != 1 {
		t.Fatalf("expected 1 join, got %d", len(sel.Joins))
	}
	j := sel.Joins[0]
	if j.Type != InnerJoin {
		t.Errorf("join type = %q, want INNER", j.Type)
	}
	if j.Table != "orders" || j.Alias != "o" {
		t.Errorf("join table/alias = %q/%q, want orders/o", j.Table, j.Alias)
	}
}

func TestSelectLeftJoin(t *testing.T) {
	stmt := parse(t, "SELECT u.name FROM users u LEFT JOIN orders o ON u.id = o.user_id")
	sel := stmt.(*SelectStatement)
	if sel.Joins[0].Type != LeftJoin {
		t.Errorf("join type = %q, want LEFT", sel.Joins[0].Type)
	}
}

// ─── INSERT ───────────────────────────────────────────────────────────────────

func TestInsertWithColumns(t *testing.T) {
	stmt := parse(t, "INSERT INTO users (id, name) VALUES (1, 'Alice')")
	ins := stmt.(*InsertStatement)
	if ins.Table != "users" {
		t.Errorf("Table = %q, want users", ins.Table)
	}
	if len(ins.Columns) != 2 || ins.Columns[0] != "id" || ins.Columns[1] != "name" {
		t.Errorf("Columns = %v, want [id name]", ins.Columns)
	}
	if len(ins.Values) != 2 {
		t.Fatalf("expected 2 values, got %d", len(ins.Values))
	}
	if ins.Values[0].(int64) != 1 {
		t.Errorf("Values[0] = %v, want 1", ins.Values[0])
	}
	if ins.Values[1].(string) != "Alice" {
		t.Errorf("Values[1] = %v, want Alice", ins.Values[1])
	}
}

func TestInsertWithoutColumns(t *testing.T) {
	stmt := parse(t, "INSERT INTO users VALUES (2, 'Bob', 24)")
	ins := stmt.(*InsertStatement)
	if len(ins.Columns) != 0 {
		t.Errorf("expected no columns, got %v", ins.Columns)
	}
	if len(ins.Values) != 3 {
		t.Errorf("expected 3 values, got %d", len(ins.Values))
	}
}

// ─── UPDATE ───────────────────────────────────────────────────────────────────

func TestUpdate(t *testing.T) {
	stmt := parse(t, "UPDATE users SET age = 31 WHERE id = 1")
	upd := stmt.(*UpdateStatement)
	if upd.Table != "users" {
		t.Errorf("Table = %q, want users", upd.Table)
	}
	if upd.Assignments["age"].(int64) != 31 {
		t.Errorf("age assignment = %v, want 31", upd.Assignments["age"])
	}
	if upd.Where == nil {
		t.Error("Where should not be nil")
	}
}

func TestUpdateNoWhere(t *testing.T) {
	stmt := parse(t, "UPDATE users SET active = TRUE")
	upd := stmt.(*UpdateStatement)
	if upd.Where != nil {
		t.Error("Where should be nil")
	}
}

// ─── DELETE ───────────────────────────────────────────────────────────────────

func TestDelete(t *testing.T) {
	stmt := parse(t, "DELETE FROM users WHERE age < 18")
	del := stmt.(*DeleteStatement)
	if del.Table != "users" {
		t.Errorf("Table = %q, want users", del.Table)
	}
	if del.Where == nil {
		t.Error("Where should not be nil")
	}
}

func TestDeleteNoWhere(t *testing.T) {
	stmt := parse(t, "DELETE FROM users")
	del := stmt.(*DeleteStatement)
	if del.Where != nil {
		t.Error("Where should be nil for delete without WHERE")
	}
}

// ─── CREATE / DROP TABLE ──────────────────────────────────────────────────────

func TestCreateTable(t *testing.T) {
	stmt := parse(t, "CREATE TABLE users (id INT PRIMARY KEY, name TEXT, age INT, active BOOLEAN)")
	ct := stmt.(*CreateTableStatement)
	if ct.Table != "users" {
		t.Errorf("Table = %q, want users", ct.Table)
	}
	if len(ct.Columns) != 4 {
		t.Fatalf("expected 4 columns, got %d", len(ct.Columns))
	}
	if ct.Columns[0].Name != "id" || !ct.Columns[0].Primary {
		t.Errorf("first column = %+v, want {id INT PRIMARY}", ct.Columns[0])
	}
	if ct.Columns[1].Type != "TEXT" {
		t.Errorf("name column type = %q, want TEXT", ct.Columns[1].Type)
	}
}

func TestDropTable(t *testing.T) {
	stmt := parse(t, "DROP TABLE users")
	dt := stmt.(*DropTableStatement)
	if dt.Table != "users" || dt.IfExists {
		t.Errorf("got %+v, want {users false}", dt)
	}
}

func TestDropTableIfExists(t *testing.T) {
	stmt := parse(t, "DROP TABLE IF EXISTS users")
	dt := stmt.(*DropTableStatement)
	if !dt.IfExists {
		t.Error("IfExists should be true")
	}
}

// ─── CREATE / DROP INDEX ──────────────────────────────────────────────────────

func TestCreateIndex(t *testing.T) {
	stmt := parse(t, "CREATE INDEX idx_level ON players (level)")
	ci := stmt.(*CreateIndexStatement)
	if ci.Name != "idx_level" || ci.Table != "players" || ci.Column != "level" {
		t.Errorf("got %+v", ci)
	}
}

func TestDropIndex(t *testing.T) {
	stmt := parse(t, "DROP INDEX idx_level")
	di := stmt.(*DropIndexStatement)
	if di.Name != "idx_level" || di.IfExists {
		t.Errorf("got %+v", di)
	}
}

// ─── ANALYZE / VACUUM ─────────────────────────────────────────────────────────

func TestAnalyze(t *testing.T) {
	stmt := parse(t, "ANALYZE players")
	a := stmt.(*AnalyzeStatement)
	if a.Table != "players" {
		t.Errorf("Table = %q, want players", a.Table)
	}
}

func TestVacuum(t *testing.T) {
	stmt := parse(t, "VACUUM players")
	v := stmt.(*VacuumStatement)
	if v.Table != "players" {
		t.Errorf("Table = %q, want players", v.Table)
	}
}

// ─── Transaction ──────────────────────────────────────────────────────────────

func TestBegin(t *testing.T) {
	stmt := parse(t, "BEGIN")
	if _, ok := stmt.(*BeginStatement); !ok {
		t.Errorf("expected *BeginStatement, got %T", stmt)
	}
}

func TestCommit(t *testing.T) {
	stmt := parse(t, "COMMIT")
	if _, ok := stmt.(*CommitStatement); !ok {
		t.Errorf("expected *CommitStatement, got %T", stmt)
	}
}

func TestRollback(t *testing.T) {
	stmt := parse(t, "ROLLBACK")
	if _, ok := stmt.(*RollbackStatement); !ok {
		t.Errorf("expected *RollbackStatement, got %T", stmt)
	}
}

// ─── EXPLAIN ──────────────────────────────────────────────────────────────────

func TestExplain(t *testing.T) {
	stmt := parse(t, "EXPLAIN SELECT * FROM users")
	ex := stmt.(*ExplainStatement)
	if ex.Analyze {
		t.Error("Analyze should be false")
	}
	if _, ok := ex.Stmt.(*SelectStatement); !ok {
		t.Errorf("inner stmt = %T, want *SelectStatement", ex.Stmt)
	}
}

func TestExplainAnalyze(t *testing.T) {
	stmt := parse(t, "EXPLAIN ANALYZE SELECT * FROM users")
	ex := stmt.(*ExplainStatement)
	if !ex.Analyze {
		t.Error("Analyze should be true")
	}
}

// ─── Literal types ────────────────────────────────────────────────────────────

func TestFloatLiteral(t *testing.T) {
	stmt := parse(t, "SELECT * FROM t WHERE score > 3.14")
	sel := stmt.(*SelectStatement)
	bin := sel.Where.(*BinaryExpr)
	lit := bin.Right.(*LiteralExpr)
	if _, ok := lit.Value.(float64); !ok {
		t.Errorf("expected float64, got %T", lit.Value)
	}
}

func TestStringLiteral(t *testing.T) {
	stmt := parse(t, "SELECT * FROM t WHERE name = 'Alice'")
	sel := stmt.(*SelectStatement)
	bin := sel.Where.(*BinaryExpr)
	lit := bin.Right.(*LiteralExpr)
	if lit.Value.(string) != "Alice" {
		t.Errorf("string literal = %v, want Alice", lit.Value)
	}
}

func TestBoolLiteral(t *testing.T) {
	stmt := parse(t, "SELECT * FROM t WHERE active = TRUE")
	sel := stmt.(*SelectStatement)
	bin := sel.Where.(*BinaryExpr)
	lit := bin.Right.(*LiteralExpr)
	if lit.Value.(bool) != true {
		t.Errorf("bool literal = %v, want true", lit.Value)
	}
}

// ─── Error cases ──────────────────────────────────────────────────────────────

func TestParseError(t *testing.T) {
	if err := parseErr("GARBAGE TOKENS HERE"); err == nil {
		t.Error("expected parse error for invalid SQL")
	}
}
