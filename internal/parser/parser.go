package parser

import (
	"database/internal/lexer"
	"fmt"
	"strconv"
	"strings"
)

type Parser struct {
	tokens []lexer.Token
	pos    int
}

func New(tokens []lexer.Token) *Parser {
	return &Parser{tokens: tokens}
}

func (p *Parser) Parse() (Statement, error) {
	switch p.current().Type {
	case lexer.SELECT:
		return p.parseSelect()
	case lexer.INSERT:
		return p.parseInsert()
	case lexer.UPDATE:
		return p.parseUpdate()
	case lexer.DELETE:
		return p.parseDelete()
	case lexer.CREATE:
		return p.parseCreate()
	case lexer.DROP:
		return p.parseDrop()
	case lexer.EXPLAIN:
		return p.parseExplain()
	default:
		return nil, fmt.Errorf("unexpected token %q — expected SELECT, INSERT, UPDATE, DELETE, CREATE, DROP, or EXPLAIN", p.current().Literal)
	}
}

// --- token primitives ---

func (p *Parser) current() lexer.Token {
	if p.pos >= len(p.tokens) {
		return lexer.Token{Type: lexer.EOF}
	}
	return p.tokens[p.pos]
}

func (p *Parser) advance() lexer.Token {
	t := p.current()
	p.pos++
	return t
}

func (p *Parser) expect(tt lexer.TokenType) (lexer.Token, error) {
	t := p.current()
	if t.Type != tt {
		return t, fmt.Errorf("expected token type %d, got %q", tt, t.Literal)
	}
	p.pos++
	return t, nil
}

func (p *Parser) is(tt lexer.TokenType) bool {
	return p.current().Type == tt
}

// --- shared helpers ---

func isAggFunc(name string) bool {
	switch strings.ToUpper(name) {
	case "COUNT", "SUM", "AVG", "MIN", "MAX":
		return true
	}
	return false
}

// parseOptionalAlias consumes [AS] ident or a bare ident alias, falling back to fallback.
func (p *Parser) parseOptionalAlias(fallback, ctx string) (string, error) {
	if p.is(lexer.AS) {
		p.advance()
		a, err := p.expect(lexer.IDENT)
		if err != nil {
			return "", fmt.Errorf("%s: expected alias after AS", ctx)
		}
		return a.Literal, nil
	}
	if p.is(lexer.IDENT) {
		return p.advance().Literal, nil
	}
	return fallback, nil
}

// parseOptionalWhere consumes WHERE expr if present.
func (p *Parser) parseOptionalWhere() (Expression, error) {
	if !p.is(lexer.WHERE) {
		return nil, nil
	}
	p.advance()
	return p.parseExpression()
}

// parseAggParen consumes (col_or_star) for an aggregate and returns the arg string.
func (p *Parser) parseAggParen(fn string) (string, error) {
	p.advance() // consume (
	arg := "*"
	if p.is(lexer.ASTERISK) {
		p.advance()
	} else {
		col, err := p.expect(lexer.IDENT)
		if err != nil {
			return "", fmt.Errorf("%s: expected column or *", fn)
		}
		arg = col.Literal
		if p.is(lexer.DOT) {
			p.advance()
			col2, err := p.expect(lexer.IDENT)
			if err != nil {
				return "", fmt.Errorf("%s: expected column after .", fn)
			}
			arg = col.Literal + "." + col2.Literal
		}
	}
	if _, err := p.expect(lexer.RPAREN); err != nil {
		return "", fmt.Errorf("%s: expected )", fn)
	}
	return arg, nil
}

// parseIntKeyword consumes keyword + integer literal. Returns nil if keyword not present.
func (p *Parser) parseIntKeyword(kw lexer.TokenType, name string) (*int64, error) {
	if !p.is(kw) {
		return nil, nil
	}
	p.advance()
	val, err := p.parseLiteral()
	if err != nil {
		return nil, fmt.Errorf("%s: expected integer", name)
	}
	n, ok := val.(int64)
	if !ok {
		return nil, fmt.Errorf("%s: expected integer, got %T", name, val)
	}
	return &n, nil
}

// --- statement parsers ---

func (p *Parser) parseSelect() (*SelectStatement, error) {
	p.advance() // consume SELECT
	stmt := &SelectStatement{}

	if p.is(lexer.DISTINCT) {
		stmt.Distinct = true
		p.advance()
	}

	if p.is(lexer.ASTERISK) {
		p.advance() // SELECT *
	} else {
		for {
			expr, err := p.parseSelectColumn()
			if err != nil {
				return nil, err
			}
			stmt.Exprs = append(stmt.Exprs, expr)
			if !p.is(lexer.COMMA) {
				break
			}
			p.advance()
		}
	}

	if _, err := p.expect(lexer.FROM); err != nil {
		return nil, fmt.Errorf("SELECT: expected FROM")
	}
	t, err := p.expect(lexer.IDENT)
	if err != nil {
		return nil, fmt.Errorf("SELECT: expected table name")
	}
	stmt.Table = t.Literal
	if stmt.Alias, err = p.parseOptionalAlias(t.Literal, "SELECT FROM"); err != nil {
		return nil, err
	}

	for p.is(lexer.JOIN) || p.is(lexer.INNER) || p.is(lexer.LEFT) {
		join, err := p.parseJoin()
		if err != nil {
			return nil, err
		}
		stmt.Joins = append(stmt.Joins, join)
	}

	if stmt.Where, err = p.parseOptionalWhere(); err != nil {
		return nil, err
	}

	if p.is(lexer.GROUP) {
		p.advance()
		if _, err := p.expect(lexer.BY); err != nil {
			return nil, fmt.Errorf("SELECT: expected BY after GROUP")
		}
		for {
			col, err := p.expect(lexer.IDENT)
			if err != nil {
				return nil, fmt.Errorf("GROUP BY: expected column name")
			}
			stmt.GroupBy = append(stmt.GroupBy, col.Literal)
			if !p.is(lexer.COMMA) {
				break
			}
			p.advance()
		}
	}

	if p.is(lexer.HAVING) {
		p.advance()
		if stmt.Having, err = p.parseExpression(); err != nil {
			return nil, err
		}
	}

	if p.is(lexer.ORDER) {
		p.advance()
		if _, err := p.expect(lexer.BY); err != nil {
			return nil, fmt.Errorf("SELECT: expected BY after ORDER")
		}
		for {
			col, err := p.parseOrderByCol()
			if err != nil {
				return nil, err
			}
			stmt.OrderBy = append(stmt.OrderBy, col)
			if !p.is(lexer.COMMA) {
				break
			}
			p.advance()
		}
	}

	if stmt.Limit, err = p.parseIntKeyword(lexer.LIMIT, "LIMIT"); err != nil {
		return nil, err
	}
	if stmt.Offset, err = p.parseIntKeyword(lexer.OFFSET, "OFFSET"); err != nil {
		return nil, err
	}

	return stmt, nil
}

func (p *Parser) parseOrderByCol() (OrderByExpr, error) {
	t, err := p.expect(lexer.IDENT)
	if err != nil {
		return OrderByExpr{}, fmt.Errorf("ORDER BY: expected column name")
	}
	col := t.Literal
	if p.is(lexer.LPAREN) && isAggFunc(col) {
		fn := strings.ToUpper(col)
		arg, err := p.parseAggParen(fn)
		if err != nil {
			return OrderByExpr{}, err
		}
		col = fn + "(" + arg + ")"
	}
	desc := false
	if p.is(lexer.DESC) {
		desc = true
		p.advance()
	} else if p.is(lexer.ASC) {
		p.advance()
	}
	return OrderByExpr{Col: col, Desc: desc}, nil
}

func (p *Parser) parseSelectColumn() (SelectExpr, error) {
	t, err := p.expect(lexer.IDENT)
	if err != nil {
		return nil, fmt.Errorf("SELECT: expected column name or aggregate, got %q", p.current().Literal)
	}

	if p.is(lexer.LPAREN) && isAggFunc(t.Literal) {
		fn := strings.ToUpper(t.Literal)
		arg, err := p.parseAggParen(fn)
		if err != nil {
			return nil, err
		}
		return &AggSelectExpr{Func: fn, Arg: arg}, nil
	}

	if p.is(lexer.DOT) {
		p.advance()
		if p.is(lexer.ASTERISK) {
			p.advance()
			return &ColSelectExpr{Col: t.Literal + ".*"}, nil
		}
		col, err := p.expect(lexer.IDENT)
		if err != nil {
			return nil, fmt.Errorf("SELECT: expected column name after %q.", t.Literal)
		}
		return &ColSelectExpr{Col: t.Literal + "." + col.Literal}, nil
	}
	return &ColSelectExpr{Col: t.Literal}, nil
}

func (p *Parser) parseJoin() (JoinClause, error) {
	jt := InnerJoin
	if p.is(lexer.LEFT) {
		jt = LeftJoin
		p.advance()
		if p.is(lexer.OUTER) {
			p.advance()
		}
	} else if p.is(lexer.INNER) {
		p.advance()
	}
	if _, err := p.expect(lexer.JOIN); err != nil {
		return JoinClause{}, fmt.Errorf("JOIN: expected JOIN keyword")
	}
	tbl, err := p.expect(lexer.IDENT)
	if err != nil {
		return JoinClause{}, fmt.Errorf("JOIN: expected table name")
	}
	alias, err := p.parseOptionalAlias(tbl.Literal, "JOIN")
	if err != nil {
		return JoinClause{}, err
	}
	if _, err := p.expect(lexer.ON); err != nil {
		return JoinClause{}, fmt.Errorf("JOIN: expected ON")
	}
	cond, err := p.parseExpression()
	if err != nil {
		return JoinClause{}, err
	}
	return JoinClause{Type: jt, Table: tbl.Literal, Alias: alias, Condition: cond}, nil
}

func (p *Parser) parseInsert() (*InsertStatement, error) {
	p.advance() // consume INSERT
	if _, err := p.expect(lexer.INTO); err != nil {
		return nil, fmt.Errorf("INSERT: expected INTO")
	}
	t, err := p.expect(lexer.IDENT)
	if err != nil {
		return nil, fmt.Errorf("INSERT: expected table name")
	}
	stmt := &InsertStatement{Table: t.Literal}

	if p.is(lexer.LPAREN) {
		p.advance()
		for !p.is(lexer.RPAREN) && !p.is(lexer.EOF) {
			col, err := p.expect(lexer.IDENT)
			if err != nil {
				return nil, fmt.Errorf("INSERT: expected column name")
			}
			stmt.Columns = append(stmt.Columns, col.Literal)
			if p.is(lexer.COMMA) {
				p.advance()
			}
		}
		if _, err := p.expect(lexer.RPAREN); err != nil {
			return nil, fmt.Errorf("INSERT: expected ) after column list")
		}
	}

	if _, err := p.expect(lexer.VALUES); err != nil {
		return nil, fmt.Errorf("INSERT: expected VALUES")
	}
	if _, err := p.expect(lexer.LPAREN); err != nil {
		return nil, fmt.Errorf("INSERT: expected ( before values")
	}
	for !p.is(lexer.RPAREN) && !p.is(lexer.EOF) {
		val, err := p.parseLiteral()
		if err != nil {
			return nil, fmt.Errorf("INSERT: %w", err)
		}
		stmt.Values = append(stmt.Values, val)
		if p.is(lexer.COMMA) {
			p.advance()
		}
	}
	if _, err := p.expect(lexer.RPAREN); err != nil {
		return nil, fmt.Errorf("INSERT: expected ) after values")
	}
	return stmt, nil
}

func (p *Parser) parseUpdate() (*UpdateStatement, error) {
	p.advance() // consume UPDATE
	t, err := p.expect(lexer.IDENT)
	if err != nil {
		return nil, fmt.Errorf("UPDATE: expected table name")
	}
	stmt := &UpdateStatement{Table: t.Literal, Assignments: make(map[string]interface{})}

	if _, err := p.expect(lexer.SET); err != nil {
		return nil, fmt.Errorf("UPDATE: expected SET")
	}
	for {
		col, err := p.expect(lexer.IDENT)
		if err != nil {
			return nil, fmt.Errorf("UPDATE: expected column name")
		}
		if _, err := p.expect(lexer.EQ); err != nil {
			return nil, fmt.Errorf("UPDATE: expected = after column %q", col.Literal)
		}
		val, err := p.parseLiteral()
		if err != nil {
			return nil, fmt.Errorf("UPDATE: %w", err)
		}
		stmt.Assignments[col.Literal] = val
		if !p.is(lexer.COMMA) {
			break
		}
		p.advance()
	}
	if stmt.Where, err = p.parseOptionalWhere(); err != nil {
		return nil, err
	}
	return stmt, nil
}

func (p *Parser) parseDelete() (*DeleteStatement, error) {
	p.advance() // consume DELETE
	if _, err := p.expect(lexer.FROM); err != nil {
		return nil, fmt.Errorf("DELETE: expected FROM")
	}
	t, err := p.expect(lexer.IDENT)
	if err != nil {
		return nil, fmt.Errorf("DELETE: expected table name")
	}
	where, err := p.parseOptionalWhere()
	if err != nil {
		return nil, err
	}
	return &DeleteStatement{Table: t.Literal, Where: where}, nil
}

func (p *Parser) parseCreate() (Statement, error) {
	p.advance() // consume CREATE
	if p.is(lexer.INDEX) {
		return p.parseCreateIndex()
	}
	if _, err := p.expect(lexer.TABLE); err != nil {
		return nil, fmt.Errorf("CREATE: expected TABLE or INDEX")
	}
	t, err := p.expect(lexer.IDENT)
	if err != nil {
		return nil, fmt.Errorf("CREATE: expected table name")
	}
	stmt := &CreateTableStatement{Table: t.Literal}

	if _, err := p.expect(lexer.LPAREN); err != nil {
		return nil, fmt.Errorf("CREATE TABLE: expected (")
	}
	for !p.is(lexer.RPAREN) && !p.is(lexer.EOF) {
		col, err := p.parseColumnDef()
		if err != nil {
			return nil, err
		}
		stmt.Columns = append(stmt.Columns, col)
		if p.is(lexer.COMMA) {
			p.advance()
		}
	}
	if _, err := p.expect(lexer.RPAREN); err != nil {
		return nil, fmt.Errorf("CREATE TABLE: expected )")
	}
	return stmt, nil
}

func (p *Parser) parseCreateIndex() (*CreateIndexStatement, error) {
	p.advance() // consume INDEX
	name, err := p.expect(lexer.IDENT)
	if err != nil {
		return nil, fmt.Errorf("CREATE INDEX: expected index name")
	}
	if _, err := p.expect(lexer.ON); err != nil {
		return nil, fmt.Errorf("CREATE INDEX: expected ON")
	}
	table, err := p.expect(lexer.IDENT)
	if err != nil {
		return nil, fmt.Errorf("CREATE INDEX: expected table name")
	}
	if _, err := p.expect(lexer.LPAREN); err != nil {
		return nil, fmt.Errorf("CREATE INDEX: expected (")
	}
	col, err := p.expect(lexer.IDENT)
	if err != nil {
		return nil, fmt.Errorf("CREATE INDEX: expected column name")
	}
	if _, err := p.expect(lexer.RPAREN); err != nil {
		return nil, fmt.Errorf("CREATE INDEX: expected )")
	}
	return &CreateIndexStatement{Name: name.Literal, Table: table.Literal, Column: col.Literal}, nil
}

func (p *Parser) parseColumnDef() (ColumnDef, error) {
	name, err := p.expect(lexer.IDENT)
	if err != nil {
		return ColumnDef{}, fmt.Errorf("column def: expected column name")
	}
	col := ColumnDef{Name: name.Literal, Type: strings.ToUpper(p.advance().Literal)}
	if p.is(lexer.PRIMARY) {
		p.advance()
		if _, err := p.expect(lexer.KEY); err != nil {
			return ColumnDef{}, fmt.Errorf("column def: expected KEY after PRIMARY")
		}
		col.Primary = true
	}
	return col, nil
}

func (p *Parser) parseDrop() (Statement, error) {
	p.advance() // consume DROP
	if p.is(lexer.INDEX) {
		return p.parseDropIndex()
	}
	if _, err := p.expect(lexer.TABLE); err != nil {
		return nil, fmt.Errorf("DROP: expected TABLE or INDEX")
	}
	ifExists := false
	if p.is(lexer.IF) {
		p.advance()
		if _, err := p.expect(lexer.EXISTS); err != nil {
			return nil, fmt.Errorf("DROP: expected EXISTS after IF")
		}
		ifExists = true
	}
	t, err := p.expect(lexer.IDENT)
	if err != nil {
		return nil, fmt.Errorf("DROP: expected table name")
	}
	return &DropTableStatement{Table: t.Literal, IfExists: ifExists}, nil
}

func (p *Parser) parseDropIndex() (*DropIndexStatement, error) {
	p.advance() // consume INDEX
	ifExists := false
	if p.is(lexer.IF) {
		p.advance()
		if _, err := p.expect(lexer.EXISTS); err != nil {
			return nil, fmt.Errorf("DROP INDEX: expected EXISTS after IF")
		}
		ifExists = true
	}
	name, err := p.expect(lexer.IDENT)
	if err != nil {
		return nil, fmt.Errorf("DROP INDEX: expected index name")
	}
	return &DropIndexStatement{Name: name.Literal, IfExists: ifExists}, nil
}

func (p *Parser) parseExplain() (*ExplainStatement, error) {
	p.advance() // consume EXPLAIN
	analyze := false
	if p.is(lexer.ANALYZE) {
		analyze = true
		p.advance()
	}
	inner, err := p.Parse()
	if err != nil {
		return nil, fmt.Errorf("EXPLAIN: %w", err)
	}
	return &ExplainStatement{Analyze: analyze, Stmt: inner}, nil
}

// --- expression parsing: OR > AND > comparison > primary ---

func (p *Parser) parseExpression() (Expression, error) { return p.parseOr() }

func (p *Parser) parseOr() (Expression, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.is(lexer.OR) {
		p.advance()
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = &BinaryExpr{Left: left, Op: "OR", Right: right}
	}
	return left, nil
}

func (p *Parser) parseAnd() (Expression, error) {
	left, err := p.parseComparison()
	if err != nil {
		return nil, err
	}
	for p.is(lexer.AND) {
		p.advance()
		right, err := p.parseComparison()
		if err != nil {
			return nil, err
		}
		left = &BinaryExpr{Left: left, Op: "AND", Right: right}
	}
	return left, nil
}

func (p *Parser) parseComparison() (Expression, error) {
	left, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	switch p.current().Type {
	case lexer.EQ, lexer.NEQ, lexer.LT, lexer.GT, lexer.LTE, lexer.GTE:
		op := p.advance().Literal
		right, err := p.parsePrimary()
		if err != nil {
			return nil, err
		}
		return &BinaryExpr{Left: left, Op: op, Right: right}, nil
	}
	return left, nil
}

func (p *Parser) parsePrimary() (Expression, error) {
	t := p.current()
	if p.is(lexer.IDENT) {
		p.advance()
		if p.is(lexer.LPAREN) && isAggFunc(t.Literal) {
			fn := strings.ToUpper(t.Literal)
			p.advance() // consume (
			var arg Expression
			if p.is(lexer.ASTERISK) {
				p.advance()
			} else {
				var err error
				if arg, err = p.parseExpression(); err != nil {
					return nil, err
				}
			}
			if _, err := p.expect(lexer.RPAREN); err != nil {
				return nil, fmt.Errorf("%s(): expected )", fn)
			}
			return &AggFuncExpr{Func: fn, Arg: arg}, nil
		}
		if p.is(lexer.DOT) {
			p.advance()
			col, err := p.expect(lexer.IDENT)
			if err != nil {
				return nil, fmt.Errorf("expected column name after %q.", t.Literal)
			}
			return &IdentExpr{Table: t.Literal, Name: col.Literal}, nil
		}
		return &IdentExpr{Name: t.Literal}, nil
	}
	val, err := p.parseLiteral()
	if err != nil {
		return nil, err
	}
	return &LiteralExpr{Value: val}, nil
}

func (p *Parser) parseLiteral() (interface{}, error) {
	t := p.advance()
	switch t.Type {
	case lexer.INT_LIT:
		n, err := strconv.ParseInt(t.Literal, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid integer %q", t.Literal)
		}
		return n, nil
	case lexer.FLOAT_LIT:
		f, err := strconv.ParseFloat(t.Literal, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid float %q", t.Literal)
		}
		return f, nil
	case lexer.STRING_LIT:
		return t.Literal, nil
	case lexer.TRUE:
		return true, nil
	case lexer.FALSE:
		return false, nil
	case lexer.NULL:
		return nil, nil
	default:
		return nil, fmt.Errorf("expected literal value, got %q", t.Literal)
	}
}
