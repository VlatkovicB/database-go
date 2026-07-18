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

func (e *ColSelectExpr) selectExprNode() {}
func (e *AggSelectExpr) selectExprNode() {}

type JoinType string

const (
	InnerJoin JoinType = "INNER"
	LeftJoin  JoinType = "LEFT"
)

type JoinClause struct {
	Type      JoinType
	Table     string
	Alias     string
	Condition Expression
}

type SelectStatement struct {
	Exprs   []SelectExpr // nil = SELECT *
	Table   string
	Alias   string // defaults to table name
	Joins   []JoinClause
	Where   Expression
	GroupBy []string
	Having  Expression
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

type CreateTableStatement struct {
	Table   string
	Columns []ColumnDef
}

type DropTableStatement struct {
	Table string
}

func (s *SelectStatement) statementNode()      {}
func (s *InsertStatement) statementNode()      {}
func (s *UpdateStatement) statementNode()      {}
func (s *DeleteStatement) statementNode()      {}
func (s *CreateTableStatement) statementNode() {}
func (s *DropTableStatement) statementNode()   {}

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

func (e *BinaryExpr) expressionNode()  {}
func (e *IdentExpr) expressionNode()   {}
func (e *LiteralExpr) expressionNode() {}
func (e *AggFuncExpr) expressionNode() {}
