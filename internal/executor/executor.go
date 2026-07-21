package executor

import (
	"database/internal/parser"
	"database/internal/storage"
	"fmt"
)

type IndexSuggestion struct {
	Reason string `json:"reason"`
	SQL    string `json:"sql"`
}

type Result struct {
	Columns          []string
	Rows             [][]interface{}
	Message          string
	Trace            []string
	StepLog          []StepEvent
	NodeTree         *NodeTreeDesc
	StepTruncated    bool
	IndexSuggestions []IndexSuggestion
}

type NodeTreeDesc struct {
	ID       int             `json:"id"`
	NodeType string          `json:"nodeType"`
	Children []*NodeTreeDesc `json:"children,omitempty"`
}

func buildNodeTree(node Node) *NodeTreeDesc {
	if node == nil {
		return nil
	}
	desc := &NodeTreeDesc{ID: node.NodeID(), NodeType: node.NodeName()}
	for _, child := range node.NodeChildren() {
		desc.Children = append(desc.Children, buildNodeTree(child))
	}
	return desc
}

type Executor struct {
	db        *storage.Database
	CurrentTx *storage.Transaction
}

func New(db *storage.Database) *Executor {
	return &Executor{db: db}
}

// currentXID returns the active transaction ID, or 0 for auto-commit mode.
func (e *Executor) currentXID() uint64 {
	if e.CurrentTx != nil {
		return e.CurrentTx.ID
	}
	return 0
}

// currentSnapshot returns a pointer to the active transaction's snapshot,
// or nil for auto-commit mode.
func (e *Executor) currentSnapshot() *storage.Snapshot {
	if e.CurrentTx != nil {
		return &e.CurrentTx.Snapshot
	}
	return nil
}

func (e *Executor) Execute(stmt parser.Statement) (*Result, error) {
	switch s := stmt.(type) {
	case *parser.SelectStatement:
		return e.execSelect(s)
	case *parser.InsertStatement:
		return e.execInsert(s)
	case *parser.UpdateStatement:
		return e.execUpdate(s)
	case *parser.DeleteStatement:
		return e.execDelete(s)
	case *parser.CreateTableStatement:
		return e.execCreate(s)
	case *parser.DropTableStatement:
		return e.execDrop(s)
	case *parser.ExplainStatement:
		return e.execExplain(s)
	case *parser.CreateIndexStatement:
		return e.execCreateIndex(s)
	case *parser.DropIndexStatement:
		return e.execDropIndex(s)
	case *parser.AnalyzeStatement:
		return e.execAnalyze(s)
	case *parser.BeginStatement:
		return e.execBegin()
	case *parser.CommitStatement:
		return e.execCommit()
	case *parser.RollbackStatement:
		return e.execRollback()
	case *parser.VacuumStatement:
		return e.execVacuum(s)
	default:
		return nil, fmt.Errorf("unknown statement type")
	}
}

func (e *Executor) execExplain(s *parser.ExplainStatement) (*Result, error) {
	if s.Analyze {
		return e.execExplainAnalyze(s.Stmt)
	}
	return e.execExplainPlan(s.Stmt)
}
