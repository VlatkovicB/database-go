package executor

import (
	"testing"
)

func TestDerivedTableFromSubquery(t *testing.T) {
	e := newExecutor()
	setupPlayers(t, e)

	// Inner WHERE level > 10 matches Carol (15) and Eve (15).
	r := exec(t, e, "SELECT t.username, t.level FROM (SELECT username, level FROM players WHERE level > 10) t ORDER BY t.level DESC")
	if len(r.Rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(r.Rows))
	}
	if r.Columns[0] != "username" || r.Columns[1] != "level" {
		t.Fatalf("unexpected columns: %v", r.Columns)
	}
	// First row should be highest level (15).
	if r.Rows[0][1] != int64(15) {
		t.Fatalf("expected level 15 first, got %v", r.Rows[0][1])
	}
}

func TestDerivedTableSelectStar(t *testing.T) {
	e := newExecutor()
	setupPlayers(t, e)

	// SELECT * from derived table.
	r := exec(t, e, "SELECT * FROM (SELECT username, level FROM players WHERE level < 10) t")
	if len(r.Rows) != 2 { // Bob(5) and Dan(8)
		t.Fatalf("expected 2 rows, got %d", len(r.Rows))
	}
}

func TestDerivedTableWithAgg(t *testing.T) {
	e := newExecutor()
	setupPlayers(t, e)

	// Derived table containing aggregated results.
	r := exec(t, e, "SELECT t.class FROM (SELECT class, COUNT(*) FROM players GROUP BY class HAVING COUNT(*) > 1) t")
	if len(r.Rows) == 0 {
		t.Fatal("expected rows from aggregated derived table")
	}
}

func TestDerivedTableJoin(t *testing.T) {
	e := newExecutor()
	setupPlayers(t, e)

	// JOIN a derived table.
	r := exec(t, e, "SELECT p.username, sub.class FROM players p JOIN (SELECT DISTINCT class FROM players WHERE level > 10) sub ON p.class = sub.class")
	if len(r.Rows) == 0 {
		t.Fatal("expected rows from derived table join")
	}
}
