package lexer

import (
	"testing"
)

func tokens(sql string) []Token {
	return New(sql).Tokenize()
}

func TestKeywords(t *testing.T) {
	tests := []struct {
		sql  string
		want []TokenType
	}{
		{"SELECT * FROM users", []TokenType{SELECT, ASTERISK, FROM, IDENT, EOF}},
		{"INSERT INTO t VALUES (1)", []TokenType{INSERT, INTO, IDENT, VALUES, LPAREN, INT_LIT, RPAREN, EOF}},
		{"UPDATE t SET a = 1 WHERE b = 2", []TokenType{UPDATE, IDENT, SET, IDENT, EQ, INT_LIT, WHERE, IDENT, EQ, INT_LIT, EOF}},
		{"DELETE FROM t WHERE id = 1", []TokenType{DELETE, FROM, IDENT, WHERE, IDENT, EQ, INT_LIT, EOF}},
		{"CREATE TABLE t (id INT PRIMARY KEY)", []TokenType{CREATE, TABLE, IDENT, LPAREN, IDENT, INT, PRIMARY, KEY, RPAREN, EOF}},
		{"DROP TABLE t", []TokenType{DROP, TABLE, IDENT, EOF}},
		{"DROP TABLE IF EXISTS t", []TokenType{DROP, TABLE, IF, EXISTS, IDENT, EOF}},
		{"BEGIN", []TokenType{BEGIN, EOF}},
		{"COMMIT", []TokenType{COMMIT, EOF}},
		{"ROLLBACK", []TokenType{ROLLBACK, EOF}},
		{"VACUUM t", []TokenType{VACUUM, IDENT, EOF}},
		{"ANALYZE t", []TokenType{ANALYZE, IDENT, EOF}},
	}
	for _, tc := range tests {
		toks := tokens(tc.sql)
		if len(toks) != len(tc.want) {
			t.Errorf("sql=%q: got %d tokens, want %d", tc.sql, len(toks), len(tc.want))
			continue
		}
		for i, tt := range tc.want {
			if toks[i].Type != tt {
				t.Errorf("sql=%q token[%d]: got %s, want %s", tc.sql, i, toks[i].Type.Name(), tt.Name())
			}
		}
	}
}

func TestCaseInsensitive(t *testing.T) {
	toks := tokens("select * from users")
	if toks[0].Type != SELECT {
		t.Errorf("expected SELECT, got %s", toks[0].Type.Name())
	}
	if toks[2].Type != FROM {
		t.Errorf("expected FROM, got %s", toks[2].Type.Name())
	}
}

func TestLiterals(t *testing.T) {
	toks := tokens("42 3.14 'hello world'")
	if toks[0].Type != INT_LIT || toks[0].Literal != "42" {
		t.Errorf("expected INT_LIT 42, got %v", toks[0])
	}
	if toks[1].Type != FLOAT_LIT || toks[1].Literal != "3.14" {
		t.Errorf("expected FLOAT_LIT 3.14, got %v", toks[1])
	}
	if toks[2].Type != STRING_LIT || toks[2].Literal != "hello world" {
		t.Errorf("expected STRING_LIT 'hello world', got %v", toks[2])
	}
}

func TestOperators(t *testing.T) {
	toks := tokens("!= <= >= < > =")
	want := []TokenType{NEQ, LTE, GTE, LT, GT, EQ, EOF}
	for i, tt := range want {
		if toks[i].Type != tt {
			t.Errorf("token[%d]: got %s, want %s", i, toks[i].Type.Name(), tt.Name())
		}
	}
}

func TestSymbols(t *testing.T) {
	toks := tokens("( ) , ; . *")
	want := []TokenType{LPAREN, RPAREN, COMMA, SEMICOLON, DOT, ASTERISK, EOF}
	for i, tt := range want {
		if toks[i].Type != tt {
			t.Errorf("token[%d]: got %s, want %s", i, toks[i].Type.Name(), tt.Name())
		}
	}
}

func TestJoinKeywords(t *testing.T) {
	toks := tokens("JOIN INNER LEFT OUTER ON AS")
	want := []TokenType{JOIN, INNER, LEFT, OUTER, ON, AS, EOF}
	for i, tt := range want {
		if toks[i].Type != tt {
			t.Errorf("token[%d]: got %s, want %s", i, toks[i].Type.Name(), tt.Name())
		}
	}
}

func TestAggregateKeywords(t *testing.T) {
	toks := tokens("GROUP BY HAVING ORDER ASC DESC DISTINCT LIMIT OFFSET")
	want := []TokenType{GROUP, BY, HAVING, ORDER, ASC, DESC, DISTINCT, LIMIT, OFFSET, EOF}
	for i, tt := range want {
		if toks[i].Type != tt {
			t.Errorf("token[%d]: got %s, want %s", i, toks[i].Type.Name(), tt.Name())
		}
	}
}

func TestBooleanLiterals(t *testing.T) {
	toks := tokens("TRUE FALSE NULL")
	want := []TokenType{TRUE, FALSE, NULL, EOF}
	for i, tt := range want {
		if toks[i].Type != tt {
			t.Errorf("token[%d]: got %s, want %s", i, toks[i].Type.Name(), tt.Name())
		}
	}
}

func TestIdentifier(t *testing.T) {
	toks := tokens("my_table col_name _private")
	for i := 0; i < 3; i++ {
		if toks[i].Type != IDENT {
			t.Errorf("token[%d]: expected IDENT, got %s", i, toks[i].Type.Name())
		}
	}
}

func TestQualifiedColumn(t *testing.T) {
	toks := tokens("u.name")
	want := []TokenType{IDENT, DOT, IDENT, EOF}
	for i, tt := range want {
		if toks[i].Type != tt {
			t.Errorf("token[%d]: got %s, want %s", i, toks[i].Type.Name(), tt.Name())
		}
	}
}

func TestTokenCategory(t *testing.T) {
	if SELECT.Category() != "keyword" {
		t.Errorf("SELECT should be keyword")
	}
	if IDENT.Category() != "ident" {
		t.Errorf("IDENT should be ident")
	}
	if INT_LIT.Category() != "literal" {
		t.Errorf("INT_LIT should be literal")
	}
	if EQ.Category() != "operator" {
		t.Errorf("EQ should be operator")
	}
	if COMMA.Category() != "punct" {
		t.Errorf("COMMA should be punct")
	}
	if INT.Category() != "type" {
		t.Errorf("INT should be type")
	}
}

func TestTokenName(t *testing.T) {
	if SELECT.Name() != "SELECT" {
		t.Errorf("SELECT.Name() = %q, want SELECT", SELECT.Name())
	}
	if EOF.Name() != "EOF" {
		t.Errorf("EOF.Name() = %q, want EOF", EOF.Name())
	}
}

func TestEmptyInput(t *testing.T) {
	toks := tokens("")
	if len(toks) != 1 || toks[0].Type != EOF {
		t.Errorf("empty input should produce only EOF, got %v", toks)
	}
}

func TestWhitespaceOnly(t *testing.T) {
	toks := tokens("   \t\n  ")
	if len(toks) != 1 || toks[0].Type != EOF {
		t.Errorf("whitespace-only input should produce only EOF, got %v", toks)
	}
}
