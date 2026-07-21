package executor

// =============================================================================
// DDL statements
// =============================================================================

import (
	"database/internal/parser"
	"database/internal/storage"
	"fmt"
)

func (e *Executor) execCreate(s *parser.CreateTableStatement) (*Result, error) {
	var cols []storage.Column
	for _, cd := range s.Columns {
		cols = append(cols, storage.Column{
			Name:    cd.Name,
			Type:    storage.ColumnType(cd.Type),
			Primary: cd.Primary,
		})
	}
	if err := e.db.CreateTable(s.Table, cols); err != nil {
		return nil, err
	}
	if len(s.ForeignKeys) > 0 {
		var fks []storage.FKConstraint
		for _, fk := range s.ForeignKeys {
			fks = append(fks, storage.FKConstraint{
				Column:    fk.Column,
				RefTable:  fk.RefTable,
				RefColumn: fk.RefColumn,
			})
		}
		if err := e.db.SetForeignKeys(s.Table, fks); err != nil {
			return nil, err
		}
	}
	trace := []string{
		fmt.Sprintf("Validate %d column definition(s)", len(cols)),
		fmt.Sprintf("Register %q in database catalog", s.Table),
		"Initialize empty row storage",
	}
	if len(s.ForeignKeys) > 0 {
		trace = append(trace, fmt.Sprintf("Register %d foreign key constraint(s)", len(s.ForeignKeys)))
	}
	return &Result{
		Message: fmt.Sprintf("table %q created", s.Table),
		Trace:   trace,
	}, nil
}

func (e *Executor) execCreateIndex(s *parser.CreateIndexStatement) (*Result, error) {
	if err := e.db.CreateIndex(s.Name, s.Table, s.Column); err != nil {
		return nil, err
	}
	return &Result{
		Message: fmt.Sprintf("index %q created on %q (%s)", s.Name, s.Table, s.Column),
		Trace: []string{
			fmt.Sprintf("Scan all pages of %q", s.Table),
			fmt.Sprintf("Build B+ tree on column %q", s.Column),
			fmt.Sprintf("Register index %q in table catalog", s.Name),
		},
	}, nil
}

func (e *Executor) execDropIndex(s *parser.DropIndexStatement) (*Result, error) {
	if err := e.db.DropIndexByName(s.Name, s.IfExists); err != nil {
		return nil, err
	}
	if s.IfExists {
		return &Result{Message: fmt.Sprintf("index %q dropped (or did not exist)", s.Name)}, nil
	}
	return &Result{
		Message: fmt.Sprintf("index %q dropped", s.Name),
		Trace: []string{
			fmt.Sprintf("Remove index %q from table catalog", s.Name),
		},
	}, nil
}

func (e *Executor) execAnalyze(s *parser.AnalyzeStatement) (*Result, error) {
	lines, err := e.db.AnalyzeTable(s.Table)
	if err != nil {
		return nil, err
	}
	return &Result{
		Message: fmt.Sprintf("ANALYZE %q — statistics updated", s.Table),
		Trace:   lines,
	}, nil
}

func (e *Executor) execDrop(s *parser.DropTableStatement) (*Result, error) {
	if err := e.db.DropTable(s.Table); err != nil {
		if s.IfExists {
			return &Result{Message: fmt.Sprintf("table %q does not exist, skipped", s.Table)}, nil
		}
		return nil, err
	}
	return &Result{
		Message: fmt.Sprintf("table %q dropped", s.Table),
		Trace: []string{
			fmt.Sprintf("Verify %q exists in catalog", s.Table),
			fmt.Sprintf("Remove %q and all its rows", s.Table),
		},
	}, nil
}
