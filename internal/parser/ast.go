package parser

// Statement is the top-level node — every SQL command implements this.
type Statement interface {
	statementNode()
}

// SelectExpr is an item in the SELECT column list.
type SelectExpr interface{ selectExprNode() }

// ColSelectExpr is a plain column reference: "col", "alias.col", "alias.*".
type ColSelectExpr struct{ Col string }

// AggSelectExpr is an aggregate in the SELECT list: COUNT(*), SUM(col), etc.
type AggSelectExpr struct {
	Func string // COUNT, SUM, AVG, MIN, MAX
	Arg  string // column name or "*"
}

// ExprSelectExpr is a scalar subquery or expression in the SELECT list with an alias.
type ExprSelectExpr struct {
	Expr  Expression
	Alias string
}

func (e *ColSelectExpr) selectExprNode()  {}
func (e *AggSelectExpr) selectExprNode()  {}
func (e *ExprSelectExpr) selectExprNode() {}

type JoinType string

const (
	InnerJoin JoinType = "INNER"
	LeftJoin  JoinType = "LEFT"
)

type JoinClause struct {
	Type         JoinType
	Table        string             // empty when JoinSubquery != nil
	Alias        string             // required alias for both table and subquery joins
	Condition    Expression
	JoinSubquery *SelectStatement   // non-nil for JOIN (SELECT ...) AS alias ON ...
	Lateral      bool               // true for JOIN LATERAL (SELECT ...) — re-evaluated per outer row
}

type OrderByExpr struct {
	Col  string // column name or aggregate key e.g. "COUNT(*)"
	Desc bool
}

// CTEDef holds one WITH clause entry.
type CTEDef struct {
	Name  string
	Query *SelectStatement
}

type SelectStatement struct {
	With         []CTEDef     // WITH clause (CTEs)
	Distinct     bool
	Exprs        []SelectExpr // nil = SELECT *
	Table        string
	Alias        string           // defaults to table name
	FromSubquery *SelectStatement // non-nil for FROM (SELECT ...) AS alias
	Joins        []JoinClause
	Where        Expression
	GroupBy      []string
	Having       Expression
	OrderBy      []OrderByExpr
	Limit        *int64
	Offset       *int64
	ForLock      string // "FOR UPDATE", "FOR SHARE", or ""
}

type InsertStatement struct {
	Table   string
	Columns []string
	Values  []interface{}
}

type UpdateStatement struct {
	Table       string
	Assignments map[string]interface{}
	Where       Expression
}

type DeleteStatement struct {
	Table string
	Where Expression
}

type ColumnDef struct {
	Name    string
	Type    string
	Primary bool
}

type ForeignKeyConstraint struct {
	Column    string
	RefTable  string
	RefColumn string
}

type CreateTableStatement struct {
	Table       string
	Columns     []ColumnDef
	ForeignKeys []ForeignKeyConstraint
}

type DropTableStatement struct {
	Table    string
	IfExists bool
}

type ExplainStatement struct {
	Analyze bool
	Stmt    Statement
}

type CreateIndexStatement struct {
	Name   string // index name
	Table  string
	Column string
}

type DropIndexStatement struct {
	Name     string // index name
	IfExists bool
}

// AnalyzeStatement computes column statistics for a table (like PostgreSQL ANALYZE).
type AnalyzeStatement struct {
	Table string
}

// Transaction control statements (MVCC Phase 5).
type BeginStatement    struct{}
type CommitStatement   struct{}
type RollbackStatement struct{}

// VacuumStatement reclaims dead tuples from a table.
type VacuumStatement struct {
	Table string
}

func (s *SelectStatement) statementNode()      {}
func (s *InsertStatement) statementNode()      {}
func (s *UpdateStatement) statementNode()      {}
func (s *DeleteStatement) statementNode()      {}
func (s *CreateTableStatement) statementNode() {}
func (s *DropTableStatement) statementNode()   {}
func (s *ExplainStatement) statementNode()     {}
func (s *CreateIndexStatement) statementNode() {}
func (s *DropIndexStatement) statementNode()   {}
func (s *AnalyzeStatement) statementNode()     {}
func (s *BeginStatement) statementNode()       {}
func (s *CommitStatement) statementNode()      {}
func (s *RollbackStatement) statementNode()    {}
func (s *VacuumStatement) statementNode()      {}

// Expression nodes used in WHERE / HAVING clauses.
type Expression interface {
	expressionNode()
}

// BinaryExpr covers comparisons (=, !=, <, >, <=, >=) and logic (AND, OR).
type BinaryExpr struct {
	Left  Expression
	Op    string
	Right Expression
}

// IdentExpr is a column reference like `age` or qualified `alias.col`.
type IdentExpr struct {
	Table string // optional alias qualifier
	Name  string
}

// LiteralExpr holds a concrete value: int64, float64, string, bool, or nil.
type LiteralExpr struct {
	Value interface{}
}

// AggFuncExpr is an aggregate function used in HAVING: COUNT(*), AVG(col), etc.
type AggFuncExpr struct {
	Func string     // COUNT, SUM, AVG, MIN, MAX
	Arg  Expression // nil means COUNT(*)
}

// SubqueryExpr is a scalar subquery used as an expression: (SELECT AVG(x) FROM t)
type SubqueryExpr struct {
	Query *SelectStatement
}

// InSubqueryExpr handles col IN (subquery) and col IN (val1, val2, ...) and NOT IN variants.
type InSubqueryExpr struct {
	Left   Expression
	Not    bool
	Query  *SelectStatement // non-nil for subquery form
	Values []Expression     // non-nil for literal list form
}

// ExistsExpr handles EXISTS (subquery) and NOT EXISTS (subquery).
type ExistsExpr struct {
	Not   bool
	Query *SelectStatement
}

func (e *BinaryExpr) expressionNode()    {}
func (e *IdentExpr) expressionNode()     {}
func (e *LiteralExpr) expressionNode()   {}
func (e *AggFuncExpr) expressionNode()   {}
func (e *SubqueryExpr) expressionNode()  {}
func (e *InSubqueryExpr) expressionNode() {}
func (e *ExistsExpr) expressionNode()    {}
