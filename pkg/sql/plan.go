// Copyright 2015 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.
//
// Author: Peter Mattis (peter@cockroachlabs.com)

package sql

import (
	"github.com/pkg/errors"
	"golang.org/x/net/context"

	"github.com/cockroachdb/cockroach/pkg/sql/parser"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/tracing"
)

type planMaker interface {
	// newPlan starts preparing the query plan for a single SQL
	// statement.
	//
	// It performs as many early checks as possible on the structure of
	// the SQL statement, including verifying permissions and type
	// checking.  The returned plan object is not ready to execute; the
	// optimizePlan() method must be called first. See makePlan()
	// below.
	//
	// This method should not be used directly; instead prefer makePlan()
	// or prepare() below.
	newPlan(
		ctx context.Context, stmt parser.Statement, desiredTypes []parser.Type,
	) (planNode, error)

	// makePlan prepares the query plan for a single SQL statement.  it
	// calls newPlan() then optimizePlan() on the result.  Execution must
	// start by calling Start() first and then iterating using Next()
	// and Values() in order to retrieve matching rows.
	//
	// makePlan starts preparing the query plan for a single SQL
	// statement.
	// It performs as many early checks as possible on the structure of
	// the SQL statement, including verifying permissions and type checking.
	// The returned plan object is ready to execute. Execution
	// must start by calling Start() first and then iterating using
	// Next() and Values() in order to retrieve matching
	// rows.
	makePlan(ctx context.Context, stmt Statement) (planNode, error)

	// prepare does the same checks as makePlan but skips building some
	// data structures necessary for execution, based on the assumption
	// that the plan will never be run. A planNode built with prepare()
	// will do just enough work to check the structural validity of the
	// SQL statement and determine types for placeholders. However it is
	// not appropriate to call optimizePlan(), Next() or Values() on a plan
	// object created with prepare().
	prepare(ctx context.Context, stmt parser.Statement) (planNode, error)
}

var _ planMaker = &planner{}

// planNode defines the interface for executing a query or portion of a query.
//
// The following methods apply to planNodes and contain special cases
// for each type; they thus need to be extended when adding/removing
// planNode instances:
// - planMaker.newPlan()
// - planMaker.prepare()
// - planMaker.setNeededColumns()  (needed_columns.go)
// - planMaker.expandPlan()        (expand_plan.go)
// - planVisitor.visit()           (walk.go)
// - planNodeNames                 (walk.go)
// - planMaker.optimizeFilters()   (filter_opt.go)
// - setLimitHint()                (limit_hint.go)
// - collectSpans()                (plan_spans.go)
// - planOrdering()                (plan_ordering.go)
// - planColumns()                 (plan_columns.go)
//
type planNode interface {
	// Start begins the processing of the query/statement and starts
	// performing side effects for data-modifying statements. Returns an
	// error if initial processing fails.
	//
	// Note: Don't use directly. Use startPlan() instead.
	//
	// Available after optimizePlan() (or makePlan).
	Start(ctx context.Context) error

	// Next performs one unit of work, returning false if an error is
	// encountered or if there is no more work to do. For statements
	// that return a result set, the Values() method will return one row
	// of results each time that Next() returns true.
	// See executor.go: countRowsAffected() and execStmt() for an example.
	//
	// Available after Start(). It is illegal to call Next() after it returns
	// false.
	Next(ctx context.Context) (bool, error)

	// Values returns the values at the current row. The result is only valid
	// until the next call to Next().
	//
	// Available after Next().
	Values() parser.Datums

	// Close terminates the planNode execution and releases its resources.
	// This method should be called if the node has been used in any way (any
	// methods on it have been called) after it was constructed. Note that this
	// doesn't imply that Start() has been necessarily called.
	Close(ctx context.Context)
}

// planNodeFastPath is implemented by nodes that can perform all their
// work during Start(), possibly affecting even multiple rows. For
// example, DELETE can do this.
type planNodeFastPath interface {
	// FastPathResults returns the affected row count and true if the
	// node has no result set and has already executed when Start() completes.
	FastPathResults() (int, bool)
}

var _ planNode = &alterTableNode{}
var _ planNode = &copyNode{}
var _ planNode = &createDatabaseNode{}
var _ planNode = &createIndexNode{}
var _ planNode = &createTableNode{}
var _ planNode = &createViewNode{}
var _ planNode = &delayedNode{}
var _ planNode = &deleteNode{}
var _ planNode = &distinctNode{}
var _ planNode = &dropDatabaseNode{}
var _ planNode = &dropIndexNode{}
var _ planNode = &dropTableNode{}
var _ planNode = &dropViewNode{}
var _ planNode = &emptyNode{}
var _ planNode = &explainDistSQLNode{}
var _ planNode = &explainPlanNode{}
var _ planNode = &traceNode{}
var _ planNode = &filterNode{}
var _ planNode = &groupNode{}
var _ planNode = &hookFnNode{}
var _ planNode = &indexJoinNode{}
var _ planNode = &insertNode{}
var _ planNode = &joinNode{}
var _ planNode = &limitNode{}
var _ planNode = &ordinalityNode{}
var _ planNode = &relocateNode{}
var _ planNode = &renderNode{}
var _ planNode = &scanNode{}
var _ planNode = &scatterNode{}
var _ planNode = &showRangesNode{}
var _ planNode = &showFingerprintsNode{}
var _ planNode = &sortNode{}
var _ planNode = &splitNode{}
var _ planNode = &unionNode{}
var _ planNode = &updateNode{}
var _ planNode = &valueGenerator{}
var _ planNode = &valuesNode{}
var _ planNode = &windowNode{}
var _ planNode = &createUserNode{}
var _ planNode = &dropUserNode{}

var _ planNodeFastPath = &deleteNode{}
var _ planNodeFastPath = &dropUserNode{}

// makePlan implements the Planner interface.
func (p *planner) makePlan(ctx context.Context, stmt Statement) (planNode, error) {
	plan, err := p.newPlan(ctx, stmt.AST, nil)
	if err != nil {
		return nil, err
	}
	if stmt.ExpectedTypes != nil {
		if !stmt.ExpectedTypes.TypesEqual(planColumns(plan)) {
			return nil, pgerror.NewError(pgerror.CodeFeatureNotSupportedError,
				"cached plan must not change result type")
		}
	}
	if err := p.semaCtx.Placeholders.AssertAllAssigned(); err != nil {
		return nil, err
	}

	needed := allColumns(plan)
	plan, err = p.optimizePlan(ctx, plan, needed)
	if err != nil {
		return nil, err
	}

	if log.V(3) {
		log.Infof(ctx, "statement %s compiled to:\n%s", stmt, planToString(ctx, plan))
	}
	return plan, nil
}

// startPlan starts the plan and all its sub-query nodes.
func (p *planner) startPlan(ctx context.Context, plan planNode) error {
	if err := p.startSubqueryPlans(ctx, plan); err != nil {
		return err
	}
	if err := plan.Start(ctx); err != nil {
		return err
	}
	// Trigger limit propagation through the plan and sub-queries.
	setUnlimited(plan)
	return nil
}

func (p *planner) maybePlanHook(ctx context.Context, stmt parser.Statement) (planNode, error) {
	// TODO(dan): This iteration makes the plan dispatch no longer constant
	// time. We could fix that with a map of `reflect.Type` but including
	// reflection in such a primary codepath is unfortunate. Instead, the
	// upcoming IR work will provide unique numeric type tags, which will
	// elegantly solve this.
	for _, planHook := range planHooks {
		if fn, header, err := planHook(stmt, p); err != nil {
			return nil, err
		} else if fn != nil {
			return &hookFnNode{f: fn, header: header}, nil
		}
	}
	return nil, nil
}

// newPlan constructs a planNode from a statement. This is used
// recursively by the various node constructors.
func (p *planner) newPlan(
	ctx context.Context, stmt parser.Statement, desiredTypes []parser.Type,
) (planNode, error) {
	tracing.AnnotateTrace()

	// This will set the system DB trigger for transactions containing
	// DDL statements that have no effect, such as
	// `BEGIN; INSERT INTO ...; CREATE TABLE IF NOT EXISTS ...; COMMIT;`
	// where the table already exists. This will generate some false
	// refreshes, but that's expected to be quite rare in practice.
	if stmt.StatementType() == parser.DDL {
		if err := p.txn.SetSystemConfigTrigger(); err != nil {
			return nil, errors.Wrap(err,
				"schema change statement cannot follow a statement that has written in the same transaction")
		}
	}

	if plan, err := p.maybePlanHook(ctx, stmt); plan != nil || err != nil {
		return plan, err
	}

	switch n := stmt.(type) {
	case *parser.AlterTable:
		return p.AlterTable(ctx, n)
	case *parser.BeginTransaction:
		return p.BeginTransaction(n)
	case *parser.CancelQuery:
		return p.CancelQuery(ctx, n)
	case *parser.CancelJob:
		return p.CancelJob(ctx, n)
	case CopyDataBlock:
		return p.CopyData(ctx, n)
	case *parser.CopyFrom:
		return p.CopyFrom(ctx, n)
	case *parser.CreateDatabase:
		return p.CreateDatabase(n)
	case *parser.CreateIndex:
		return p.CreateIndex(ctx, n)
	case *parser.CreateTable:
		return p.CreateTable(ctx, n)
	case *parser.CreateUser:
		return p.CreateUser(ctx, n)
	case *parser.CreateView:
		return p.CreateView(ctx, n)
	case *parser.Deallocate:
		return p.Deallocate(ctx, n)
	case *parser.Delete:
		return p.Delete(ctx, n, desiredTypes)
	case *parser.Discard:
		return p.Discard(ctx, n)
	case *parser.DropDatabase:
		return p.DropDatabase(ctx, n)
	case *parser.DropIndex:
		return p.DropIndex(ctx, n)
	case *parser.DropTable:
		return p.DropTable(ctx, n)
	case *parser.DropView:
		return p.DropView(ctx, n)
	case *parser.DropUser:
		return p.DropUser(ctx, n)
	case *parser.Explain:
		return p.Explain(ctx, n)
	case *parser.Grant:
		return p.Grant(ctx, n)
	case *parser.Help:
		return p.Help(ctx, n)
	case *parser.Insert:
		return p.Insert(ctx, n, desiredTypes)
	case *parser.ParenSelect:
		return p.newPlan(ctx, n.Select, desiredTypes)
	case *parser.PauseJob:
		return p.PauseJob(ctx, n)
	case *parser.Relocate:
		return p.Relocate(ctx, n)
	case *parser.RenameColumn:
		return p.RenameColumn(ctx, n)
	case *parser.RenameDatabase:
		return p.RenameDatabase(ctx, n)
	case *parser.RenameIndex:
		return p.RenameIndex(ctx, n)
	case *parser.RenameTable:
		return p.RenameTable(ctx, n)
	case *parser.ResumeJob:
		return p.ResumeJob(ctx, n)
	case *parser.Revoke:
		return p.Revoke(ctx, n)
	case *parser.Scatter:
		return p.Scatter(ctx, n)
	case *parser.Select:
		return p.Select(ctx, n, desiredTypes)
	case *parser.SelectClause:
		return p.SelectClause(ctx, n, nil, nil, desiredTypes, publicColumns)
	case *parser.Set:
		return p.Set(ctx, n)
	case *parser.SetTransaction:
		return p.SetTransaction(n)
	case *parser.SetDefaultIsolation:
		return p.SetDefaultIsolation(n)
	case *parser.Show:
		return p.Show(n)
	case *parser.ShowColumns:
		return p.ShowColumns(ctx, n)
	case *parser.ShowConstraints:
		return p.ShowConstraints(ctx, n)
	case *parser.ShowCreateTable:
		return p.ShowCreateTable(ctx, n)
	case *parser.ShowCreateView:
		return p.ShowCreateView(ctx, n)
	case *parser.ShowDatabases:
		return p.ShowDatabases(ctx, n)
	case *parser.ShowGrants:
		return p.ShowGrants(ctx, n)
	case *parser.ShowIndex:
		return p.ShowIndex(ctx, n)
	case *parser.ShowQueries:
		return p.ShowQueries(ctx, n)
	case *parser.ShowJobs:
		return p.ShowJobs(ctx, n)
	case *parser.ShowSessions:
		return p.ShowSessions(ctx, n)
	case *parser.ShowTables:
		return p.ShowTables(ctx, n)
	case *parser.ShowTrace:
		return p.ShowTrace(ctx, n)
	case *parser.ShowTransactionStatus:
		return p.ShowTransactionStatus()
	case *parser.ShowUsers:
		return p.ShowUsers(ctx, n)
	case *parser.ShowRanges:
		return p.ShowRanges(ctx, n)
	case *parser.ShowFingerprints:
		return p.ShowFingerprints(ctx, n)
	case *parser.Split:
		return p.Split(ctx, n)
	case *parser.Truncate:
		return p.Truncate(ctx, n)
	case *parser.UnionClause:
		return p.UnionClause(ctx, n, desiredTypes)
	case *parser.Update:
		return p.Update(ctx, n, desiredTypes)
	case *parser.ValuesClause:
		return p.ValuesClause(ctx, n, desiredTypes)
	default:
		return nil, errors.Errorf("unknown statement type: %T", stmt)
	}
}

// prepare constructs the logical plan for the statement.  This is
// needed both to type placeholders and to inform pgwire of the types
// of the result columns. All statements that either support
// placeholders or have result columns must be handled here.
func (p *planner) prepare(ctx context.Context, stmt parser.Statement) (planNode, error) {
	if plan, err := p.maybePlanHook(ctx, stmt); plan != nil || err != nil {
		return plan, err
	}

	switch n := stmt.(type) {
	case *parser.Delete:
		return p.Delete(ctx, n, nil)
	case *parser.Explain:
		return p.Explain(ctx, n)
	case *parser.Help:
		return p.Help(ctx, n)
	case *parser.Insert:
		return p.Insert(ctx, n, nil)
	case *parser.Select:
		return p.Select(ctx, n, nil)
	case *parser.SelectClause:
		return p.SelectClause(ctx, n, nil, nil, nil, publicColumns)
	case *parser.Show:
		return p.Show(n)
	case *parser.ShowCreateTable:
		return p.ShowCreateTable(ctx, n)
	case *parser.ShowCreateView:
		return p.ShowCreateView(ctx, n)
	case *parser.ShowColumns:
		return p.ShowColumns(ctx, n)
	case *parser.ShowDatabases:
		return p.ShowDatabases(ctx, n)
	case *parser.ShowGrants:
		return p.ShowGrants(ctx, n)
	case *parser.ShowIndex:
		return p.ShowIndex(ctx, n)
	case *parser.ShowConstraints:
		return p.ShowConstraints(ctx, n)
	case *parser.ShowQueries:
		return p.ShowQueries(ctx, n)
	case *parser.ShowJobs:
		return p.ShowJobs(ctx, n)
	case *parser.ShowSessions:
		return p.ShowSessions(ctx, n)
	case *parser.ShowTables:
		return p.ShowTables(ctx, n)
	case *parser.ShowTrace:
		return p.ShowTrace(ctx, n)
	case *parser.ShowUsers:
		return p.ShowUsers(ctx, n)
	case *parser.ShowTransactionStatus:
		return p.ShowTransactionStatus()
	case *parser.ShowRanges:
		return p.ShowRanges(ctx, n)
	case *parser.Split:
		return p.Split(ctx, n)
	case *parser.Relocate:
		return p.Relocate(ctx, n)
	case *parser.Scatter:
		return p.Scatter(ctx, n)
	case *parser.Update:
		return p.Update(ctx, n, nil)
	default:
		// Other statement types do not have result columns and do not
		// support placeholders so there is no need for any special
		// handling here.
		return nil, nil
	}
}
