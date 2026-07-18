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
	default:
		return nil, fmt.Errorf("unexpected token %q — expected SELECT, INSERT, UPDATE, DELETE, CREATE, or DROP", p.current().Literal)
	}
}

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

// parseSelect handles: SELECT cols FROM table [alias] [JOIN ...] [WHERE expr] [GROUP BY cols] [HAVING expr]
func (p *Parser) parseSelect() (*SelectStatement, error) {
	p.advance() // consume SELECT
	stmt := &SelectStatement{}

	// Optional DISTINCT
	if p.current().Type == lexer.DISTINCT {
		stmt.Distinct = true
		p.advance()
	}

	// Column list: *, alias.*, alias.col, col, COUNT(*), SUM(col), ...
	if p.current().Type == lexer.ASTERISK {
		stmt.Exprs = nil // SELECT *
		p.advance()
	} else {
		for {
			expr, err := p.parseSelectColumn()
			if err != nil {
				return nil, err
			}
			stmt.Exprs = append(stmt.Exprs, expr)
			if p.current().Type != lexer.COMMA {
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
	stmt.Alias = t.Literal // default alias = table name

	// Optional alias: FROM table [AS] alias
	if p.current().Type == lexer.AS {
		p.advance()
		a, err := p.expect(lexer.IDENT)
		if err != nil {
			return nil, fmt.Errorf("SELECT FROM: expected alias after AS")
		}
		stmt.Alias = a.Literal
	} else if p.current().Type == lexer.IDENT {
		stmt.Alias = p.advance().Literal
	}

	// JOIN clauses
	for p.current().Type == lexer.JOIN ||
		p.current().Type == lexer.INNER ||
		p.current().Type == lexer.LEFT {
		join, err := p.parseJoin()
		if err != nil {
			return nil, err
		}
		stmt.Joins = append(stmt.Joins, join)
	}

	if p.current().Type == lexer.WHERE {
		p.advance()
		expr, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		stmt.Where = expr
	}

	if p.current().Type == lexer.GROUP {
		p.advance() // consume GROUP
		if _, err := p.expect(lexer.BY); err != nil {
			return nil, fmt.Errorf("SELECT: expected BY after GROUP")
		}
		for {
			col, err := p.expect(lexer.IDENT)
			if err != nil {
				return nil, fmt.Errorf("GROUP BY: expected column name")
			}
			stmt.GroupBy = append(stmt.GroupBy, col.Literal)
			if p.current().Type != lexer.COMMA {
				break
			}
			p.advance()
		}
	}

	if p.current().Type == lexer.HAVING {
		p.advance()
		expr, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		stmt.Having = expr
	}

	if p.current().Type == lexer.ORDER {
		p.advance() // consume ORDER
		if _, err := p.expect(lexer.BY); err != nil {
			return nil, fmt.Errorf("SELECT: expected BY after ORDER")
		}
		for {
			col, err := p.parseOrderByCol()
			if err != nil {
				return nil, err
			}
			stmt.OrderBy = append(stmt.OrderBy, col)
			if p.current().Type != lexer.COMMA {
				break
			}
			p.advance()
		}
	}

	if p.current().Type == lexer.LIMIT {
		p.advance()
		val, err := p.parseLiteral()
		if err != nil {
			return nil, fmt.Errorf("LIMIT: expected integer")
		}
		n, ok := val.(int64)
		if !ok {
			return nil, fmt.Errorf("LIMIT: expected integer, got %T", val)
		}
		stmt.Limit = &n
	}

	if p.current().Type == lexer.OFFSET {
		p.advance()
		val, err := p.parseLiteral()
		if err != nil {
			return nil, fmt.Errorf("OFFSET: expected integer")
		}
		n, ok := val.(int64)
		if !ok {
			return nil, fmt.Errorf("OFFSET: expected integer, got %T", val)
		}
		stmt.Offset = &n
	}

	return stmt, nil
}

// parseOrderByCol parses one ORDER BY item: col [ASC|DESC] or aggfunc(...) [ASC|DESC]
func (p *Parser) parseOrderByCol() (OrderByExpr, error) {
	t, err := p.expect(lexer.IDENT)
	if err != nil {
		return OrderByExpr{}, fmt.Errorf("ORDER BY: expected column name")
	}
	col := t.Literal
	// Aggregate function in ORDER BY: COUNT(*), SUM(col), etc.
	if p.current().Type == lexer.LPAREN {
		fn := strings.ToUpper(col)
		switch fn {
		case "COUNT", "SUM", "AVG", "MIN", "MAX":
			p.advance() // consume (
			arg := "*"
			if p.current().Type == lexer.ASTERISK {
				p.advance()
			} else {
				c, err := p.expect(lexer.IDENT)
				if err != nil {
					return OrderByExpr{}, fmt.Errorf("ORDER BY %s: expected column or *", fn)
				}
				arg = c.Literal
			}
			if _, err := p.expect(lexer.RPAREN); err != nil {
				return OrderByExpr{}, fmt.Errorf("ORDER BY %s: expected )", fn)
			}
			col = fn + "(" + arg + ")"
		}
	}
	desc := false
	if p.current().Type == lexer.DESC {
		desc = true
		p.advance()
	} else if p.current().Type == lexer.ASC {
		p.advance()
	}
	return OrderByExpr{Col: col, Desc: desc}, nil
}

// parseSelectColumn parses a single item in the SELECT list.
// Supports: col, alias.col, alias.*, COUNT(*), SUM(col), AVG(col), MIN(col), MAX(col)
func (p *Parser) parseSelectColumn() (SelectExpr, error) {
	t, err := p.expect(lexer.IDENT)
	if err != nil {
		return nil, fmt.Errorf("SELECT: expected column name or aggregate, got %q", p.current().Literal)
	}

	// Aggregate function: IDENT(...)
	if p.current().Type == lexer.LPAREN {
		fn := strings.ToUpper(t.Literal)
		switch fn {
		case "COUNT", "SUM", "AVG", "MIN", "MAX":
			p.advance() // consume (
			arg := "*"
			if p.current().Type == lexer.ASTERISK {
				p.advance() // consume *
			} else {
				col, err := p.expect(lexer.IDENT)
				if err != nil {
					return nil, fmt.Errorf("%s: expected column or *", fn)
				}
				arg = col.Literal
				if p.current().Type == lexer.DOT {
					p.advance()
					col2, err := p.expect(lexer.IDENT)
					if err != nil {
						return nil, fmt.Errorf("%s: expected column after .", fn)
					}
					arg = col.Literal + "." + col2.Literal
				}
			}
			if _, err := p.expect(lexer.RPAREN); err != nil {
				return nil, fmt.Errorf("%s: expected )", fn)
			}
			return &AggSelectExpr{Func: fn, Arg: arg}, nil
		}
	}

	// Plain column reference: col, alias.col, alias.*
	if p.current().Type == lexer.DOT {
		p.advance() // consume dot
		if p.current().Type == lexer.ASTERISK {
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

// parseJoin handles: [INNER | LEFT [OUTER]] JOIN table [alias] ON expr
func (p *Parser) parseJoin() (JoinClause, error) {
	jt := InnerJoin
	if p.current().Type == lexer.LEFT {
		jt = LeftJoin
		p.advance()
		if p.current().Type == lexer.OUTER {
			p.advance() // optional OUTER
		}
	} else if p.current().Type == lexer.INNER {
		p.advance()
	}
	if _, err := p.expect(lexer.JOIN); err != nil {
		return JoinClause{}, fmt.Errorf("JOIN: expected JOIN keyword")
	}
	tbl, err := p.expect(lexer.IDENT)
	if err != nil {
		return JoinClause{}, fmt.Errorf("JOIN: expected table name")
	}
	alias := tbl.Literal
	if p.current().Type == lexer.AS {
		p.advance()
		a, err := p.expect(lexer.IDENT)
		if err != nil {
			return JoinClause{}, fmt.Errorf("JOIN: expected alias after AS")
		}
		alias = a.Literal
	} else if p.current().Type == lexer.IDENT {
		alias = p.advance().Literal
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

// parseInsert handles: INSERT INTO table [(cols)] VALUES (vals)
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

	// Optional column list: INSERT INTO t (a, b, c)
	if p.current().Type == lexer.LPAREN {
		p.advance()
		for p.current().Type != lexer.RPAREN && p.current().Type != lexer.EOF {
			col, err := p.expect(lexer.IDENT)
			if err != nil {
				return nil, fmt.Errorf("INSERT: expected column name")
			}
			stmt.Columns = append(stmt.Columns, col.Literal)
			if p.current().Type == lexer.COMMA {
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
	for p.current().Type != lexer.RPAREN && p.current().Type != lexer.EOF {
		val, err := p.parseLiteral()
		if err != nil {
			return nil, fmt.Errorf("INSERT: %w", err)
		}
		stmt.Values = append(stmt.Values, val)
		if p.current().Type == lexer.COMMA {
			p.advance()
		}
	}
	if _, err := p.expect(lexer.RPAREN); err != nil {
		return nil, fmt.Errorf("INSERT: expected ) after values")
	}

	return stmt, nil
}

// parseUpdate handles: UPDATE table SET col=val [, col=val] [WHERE expr]
func (p *Parser) parseUpdate() (*UpdateStatement, error) {
	p.advance() // consume UPDATE
	t, err := p.expect(lexer.IDENT)
	if err != nil {
		return nil, fmt.Errorf("UPDATE: expected table name")
	}
	stmt := &UpdateStatement{
		Table:       t.Literal,
		Assignments: make(map[string]interface{}),
	}

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
		if p.current().Type != lexer.COMMA {
			break
		}
		p.advance()
	}

	if p.current().Type == lexer.WHERE {
		p.advance()
		expr, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		stmt.Where = expr
	}

	return stmt, nil
}

// parseDelete handles: DELETE FROM table [WHERE expr]
func (p *Parser) parseDelete() (*DeleteStatement, error) {
	p.advance() // consume DELETE
	if _, err := p.expect(lexer.FROM); err != nil {
		return nil, fmt.Errorf("DELETE: expected FROM")
	}
	t, err := p.expect(lexer.IDENT)
	if err != nil {
		return nil, fmt.Errorf("DELETE: expected table name")
	}
	stmt := &DeleteStatement{Table: t.Literal}

	if p.current().Type == lexer.WHERE {
		p.advance()
		expr, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		stmt.Where = expr
	}

	return stmt, nil
}

// parseCreate handles: CREATE TABLE name (col type [PRIMARY KEY], ...)
func (p *Parser) parseCreate() (*CreateTableStatement, error) {
	p.advance() // consume CREATE
	if _, err := p.expect(lexer.TABLE); err != nil {
		return nil, fmt.Errorf("CREATE: expected TABLE")
	}
	t, err := p.expect(lexer.IDENT)
	if err != nil {
		return nil, fmt.Errorf("CREATE: expected table name")
	}
	stmt := &CreateTableStatement{Table: t.Literal}

	if _, err := p.expect(lexer.LPAREN); err != nil {
		return nil, fmt.Errorf("CREATE TABLE: expected (")
	}
	for p.current().Type != lexer.RPAREN && p.current().Type != lexer.EOF {
		col, err := p.parseColumnDef()
		if err != nil {
			return nil, err
		}
		stmt.Columns = append(stmt.Columns, col)
		if p.current().Type == lexer.COMMA {
			p.advance()
		}
	}
	if _, err := p.expect(lexer.RPAREN); err != nil {
		return nil, fmt.Errorf("CREATE TABLE: expected )")
	}

	return stmt, nil
}

func (p *Parser) parseColumnDef() (ColumnDef, error) {
	name, err := p.expect(lexer.IDENT)
	if err != nil {
		return ColumnDef{}, fmt.Errorf("column def: expected column name")
	}
	typeToken := p.advance()
	typeName := strings.ToUpper(typeToken.Literal)

	col := ColumnDef{Name: name.Literal, Type: typeName}

	if p.current().Type == lexer.PRIMARY {
		p.advance()
		if _, err := p.expect(lexer.KEY); err != nil {
			return ColumnDef{}, fmt.Errorf("column def: expected KEY after PRIMARY")
		}
		col.Primary = true
	}

	return col, nil
}

// parseDrop handles: DROP TABLE name
func (p *Parser) parseDrop() (*DropTableStatement, error) {
	p.advance() // consume DROP
	if _, err := p.expect(lexer.TABLE); err != nil {
		return nil, fmt.Errorf("DROP: expected TABLE")
	}
	t, err := p.expect(lexer.IDENT)
	if err != nil {
		return nil, fmt.Errorf("DROP: expected table name")
	}
	return &DropTableStatement{Table: t.Literal}, nil
}

// Expression parsing uses recursive descent: OR > AND > comparison > primary.
func (p *Parser) parseExpression() (Expression, error) {
	return p.parseOr()
}

func (p *Parser) parseOr() (Expression, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.current().Type == lexer.OR {
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
	for p.current().Type == lexer.AND {
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
	if t.Type == lexer.IDENT {
		p.advance()
		// Aggregate function in expression context: COUNT(*), AVG(col), etc. (used in HAVING)
		if p.current().Type == lexer.LPAREN {
			fn := strings.ToUpper(t.Literal)
			switch fn {
			case "COUNT", "SUM", "AVG", "MIN", "MAX":
				p.advance() // consume (
				var arg Expression
				if p.current().Type == lexer.ASTERISK {
					p.advance() // consume * — arg stays nil (meaning *)
				} else {
					var err error
					arg, err = p.parseExpression()
					if err != nil {
						return nil, err
					}
				}
				if _, err := p.expect(lexer.RPAREN); err != nil {
					return nil, fmt.Errorf("%s(): expected )", fn)
				}
				return &AggFuncExpr{Func: fn, Arg: arg}, nil
			}
		}
		// Check for qualified ref: alias.col
		if p.current().Type == lexer.DOT {
			p.advance() // consume dot
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
