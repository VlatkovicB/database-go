package parser

// Statement is the top-level node — every SQL command implements this.
type Statement interface {
	statementNode()
}

type SelectStatement struct {
	Columns []string // nil = SELECT *
	Table   string
	Where   Expression
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

// Expression nodes used in WHERE clauses and assignments.
type Expression interface {
	expressionNode()
}

// BinaryExpr covers comparisons (=, !=, <, >, <=, >=) and logic (AND, OR).
type BinaryExpr struct {
	Left  Expression
	Op    string
	Right Expression
}

// IdentExpr is a column reference like `age` or `name`.
type IdentExpr struct {
	Name string
}

// LiteralExpr holds a concrete value: int64, float64, string, bool, or nil.
type LiteralExpr struct {
	Value interface{}
}

func (e *BinaryExpr) expressionNode()  {}
func (e *IdentExpr) expressionNode()   {}
func (e *LiteralExpr) expressionNode() {}
