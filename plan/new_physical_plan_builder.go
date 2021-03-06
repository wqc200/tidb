// Copyright 2017 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package plan

import (
	"math"

	"github.com/juju/errors"
	"github.com/pingcap/tidb/context"
	"github.com/pingcap/tidb/expression"
	"github.com/pingcap/tidb/infoschema"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/model"
	"github.com/pingcap/tidb/mysql"
	"github.com/pingcap/tidb/util/types"
)

func (p *requiredProp) enforceProperty(task taskProfile, ctx context.Context, allocator *idAllocator) taskProfile {
	if p.isEmpty() {
		return task
	}
	sort := Sort{ByItems: make([]*ByItems, 0, len(p.cols))}.init(allocator, ctx)
	for _, col := range p.cols {
		sort.ByItems = append(sort.ByItems, &ByItems{col, p.desc})
	}
	sort.SetSchema(task.plan().Schema())
	return sort.attach2TaskProfile(task)
}

// getPushedProp will check if this sort property can be pushed or not.
// When a sort column will be replaced by scalar function, we refuse it.
// When a sort column will be replaced by a constant, we just remove it.
func (p *Projection) getPushedProp(prop *requiredProp) (*requiredProp, bool) {
	newProp := &requiredProp{}
	if prop.isEmpty() {
		return newProp, false
	}
	newCols := make([]*expression.Column, 0, len(prop.cols))
	for _, col := range prop.cols {
		idx := p.schema.ColumnIndex(col)
		if idx == -1 {
			return newProp, false
		}
		switch expr := p.Exprs[idx].(type) {
		case *expression.Column:
			newCols = append(newCols, expr)
		case *expression.ScalarFunction:
			return newProp, false
		}
	}
	newProp.cols = newCols
	newProp.desc = prop.desc
	return newProp, true
}

// convert2NewPhysicalPlan implements PhysicalPlan interface.
// If the Projection maps a scalar function to a sort column, it will refuse the prop.
// TODO: We can analyze the function dependence to propagate the required prop. e.g For a + 1 as b , we can take the order
// of b to a.
func (p *Projection) convert2NewPhysicalPlan(prop *requiredProp) (taskProfile, error) {
	task, err := p.getTaskProfile(prop)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if task != nil {
		return task, nil
	}
	// enforceProperty task.
	task, err = p.children[0].(LogicalPlan).convert2NewPhysicalPlan(&requiredProp{})
	if err != nil {
		return nil, errors.Trace(err)
	}
	task = p.attach2TaskProfile(task)
	task = prop.enforceProperty(task, p.ctx, p.allocator)

	newProp, canPassProp := p.getPushedProp(prop)
	if canPassProp {
		orderedTask, err := p.children[0].(LogicalPlan).convert2NewPhysicalPlan(newProp)
		if err != nil {
			return nil, errors.Trace(err)
		}
		orderedTask = p.attach2TaskProfile(orderedTask)
		if orderedTask.cost() < task.cost() {
			task = orderedTask
		}
	}
	return task, p.storeTaskProfile(prop, task)
}

// getPushedProp will check if this sort property can be pushed or not. In order to simplify the problem, we only
// consider the case that all expression are columns and all of them are asc or desc.
func (p *Sort) getPushedProp() (*requiredProp, bool) {
	desc := false
	cols := make([]*expression.Column, 0, len(p.ByItems))
	for i, item := range p.ByItems {
		col, ok := item.Expr.(*expression.Column)
		if !ok {
			return nil, false
		}
		cols = append(cols, col)
		desc = item.Desc
		if i > 0 && item.Desc != p.ByItems[i-1].Desc {
			return nil, false
		}
	}
	return &requiredProp{cols, desc}, true
}

// convert2NewPhysicalPlan implements PhysicalPlan interface.
// If this sort is a topN plan, we will try to push the sort down and leave the limit.
// TODO: If this is a sort plan and the coming prop is not nil, this plan is redundant and can be removed.
func (p *Sort) convert2NewPhysicalPlan(prop *requiredProp) (taskProfile, error) {
	task, err := p.getTaskProfile(prop)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if task != nil {
		return task, nil
	}
	// enforce branch
	task, err = p.children[0].(LogicalPlan).convert2NewPhysicalPlan(&requiredProp{})
	if err != nil {
		return nil, errors.Trace(err)
	}
	task = p.attach2TaskProfile(task)
	newProp, canPassProp := p.getPushedProp()
	if canPassProp {
		orderedTask, err := p.children[0].(LogicalPlan).convert2NewPhysicalPlan(newProp)
		if err != nil {
			return nil, errors.Trace(err)
		}
		// Leave the limit.
		if p.ExecLimit != nil {
			limit := Limit{Offset: p.ExecLimit.Offset, Count: p.ExecLimit.Count}.init(p.allocator, p.ctx)
			limit.SetSchema(p.schema)
			orderedTask = limit.attach2TaskProfile(orderedTask)
		}
		if orderedTask.cost() < task.cost() {
			task = orderedTask
		}
	}
	task = prop.enforceProperty(task, p.ctx, p.allocator)
	return task, p.storeTaskProfile(prop, task)
}

// canPushDown checks if this plan can be pushed down.
func planCanPushDown(p LogicalPlan) bool {
	switch v := p.(type) {
	case *Selection:
		v.splitPushDownConditions()
		return len(v.pushDownConditions) > 0
	case *Sort:
		return v.canPushDown()
	case *Limit:
		return true
	case *LogicalAggregation:
		// FIXME: We should check every expressions for gby items and function arguments.
		return true
	}
	return false
}

// convert2NewPhysicalPlan implements LogicalPlan interface.
func (p *baseLogicalPlan) convert2NewPhysicalPlan(prop *requiredProp) (taskProfile, error) {
	task, err := p.getTaskProfile(prop)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if task != nil {
		return task, nil
	}
	if len(p.basePlan.children) == 0 {
		task = &rootTaskProfile{p: p.basePlan.self.(PhysicalPlan)}
	} else {
		// enforce branch
		task, err = p.basePlan.children[0].(LogicalPlan).convert2NewPhysicalPlan(&requiredProp{})
		if err != nil {
			return nil, errors.Trace(err)
		}
	}
	task = p.basePlan.self.(PhysicalPlan).attach2TaskProfile(task)
	task = prop.enforceProperty(task, p.basePlan.ctx, p.basePlan.allocator)
	if !prop.isEmpty() && len(p.basePlan.children) > 0 {
		orderedTask, err := p.basePlan.children[0].(LogicalPlan).convert2NewPhysicalPlan(prop)
		if err != nil {
			return nil, errors.Trace(err)
		}
		orderedTask = p.basePlan.self.(PhysicalPlan).attach2TaskProfile(orderedTask)
		if orderedTask.cost() < task.cost() {
			task = orderedTask
		}
	}
	return task, p.storeTaskProfile(prop, task)
}

// convert2NewPhysicalPlan implements LogicalPlan interface.
func (p *Selection) convert2NewPhysicalPlan(prop *requiredProp) (taskProfile, error) {
	task, err := p.getTaskProfile(prop)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if task != nil {
		return task, nil
	}
	// TODO: We will do it in preparing phase in future.
	p.splitPushDownConditions()
	// enforce branch
	task, err = p.children[0].(LogicalPlan).convert2NewPhysicalPlan(&requiredProp{})
	if err != nil {
		return nil, errors.Trace(err)
	}
	task = p.attach2TaskProfile(task)
	task = prop.enforceProperty(task, p.basePlan.ctx, p.basePlan.allocator)
	if !prop.isEmpty() {
		orderedTask, err := p.basePlan.children[0].(LogicalPlan).convert2NewPhysicalPlan(prop)
		if err != nil {
			return nil, errors.Trace(err)
		}
		orderedTask = p.attach2TaskProfile(orderedTask)
		// TODO: Here is a trick: selection is the only plan that may not finish the cop task. It is unfair that we compare
		// the cost between CopTask and RootTask. So we try to finish the cop task here if its parent can finish it.
		// We can remove this check after we support join pushed down.
		if cop, ok := orderedTask.(*copTaskProfile); ok && !planCanPushDown(p.basePlan.parents[0].(LogicalPlan)) {
			orderedTask = cop.finishTask(p.basePlan.ctx, p.basePlan.allocator)
		}
		if orderedTask.cost() < task.cost() {
			task = orderedTask
		}
	}
	return task, p.storeTaskProfile(prop, task)
}

// checkMemTableAndGetTask will check if this table is a mem table. If it is, it will produce a task and store it.
func (p *DataSource) getMemTableTask(prop *requiredProp) (task taskProfile, err error) {
	client := p.ctx.GetClient()
	memDB := infoschema.IsMemoryDB(p.DBName.L)
	isDistReq := !memDB && client != nil && client.SupportRequestType(kv.ReqTypeSelect, 0)
	if isDistReq {
		return nil, nil
	}
	memTable := PhysicalMemTable{
		DBName:      p.DBName,
		Table:       p.tableInfo,
		Columns:     p.Columns,
		TableAsName: p.TableAsName,
	}.init(p.allocator, p.ctx)
	memTable.SetSchema(p.schema)
	rb := &rangeBuilder{sc: p.ctx.GetSessionVars().StmtCtx}
	memTable.Ranges = rb.buildTableRanges(fullRange)
	task = &rootTaskProfile{p: memTable}
	task = prop.enforceProperty(task, p.ctx, p.allocator)
	return task, p.storeTaskProfile(prop, task)
}

// convert2NewPhysicalPlan implements the PhysicalPlan interface.
// It will enumerate all the available indices and choose a plan with least cost.
func (p *DataSource) convert2NewPhysicalPlan(prop *requiredProp) (taskProfile, error) {
	task, err := p.getTaskProfile(prop)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if task != nil {
		return task, nil
	}
	// TODO: We don't consider the false condition here. We will add this check in PPD phase.
	task, err = p.getMemTableTask(prop)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if task != nil {
		return task, nil
	}
	// TODO: We have not checked if this table has a predicate. If not, we can only consider table scan.
	indices, includeTableScan := availableIndices(p.indexHints, p.tableInfo)
	if includeTableScan {
		task, err = p.convertToTableScan(prop)
		if err != nil {
			return nil, errors.Trace(err)
		}
	}
	for _, idx := range indices {
		idxTask, err := p.convertToIndexScan(prop, idx)
		if err != nil {
			return nil, errors.Trace(err)
		}
		if task == nil || idxTask.cost() < task.cost() {
			task = idxTask
		}
	}
	return task, p.storeTaskProfile(prop, task)
}

// convert2IndexScanner converts the DataSource to index scan with idx.
func (p *DataSource) convertToIndexScan(prop *requiredProp, idx *model.IndexInfo) (task taskProfile, err error) {
	is := PhysicalIndexScan{
		Table:       p.tableInfo,
		TableAsName: p.TableAsName,
		DBName:      p.DBName,
		Columns:     p.Columns,
		Index:       idx,
	}.init(p.allocator, p.ctx)
	statsTbl := p.statisticTable
	rowCount := float64(statsTbl.Count)
	sc := p.ctx.GetSessionVars().StmtCtx
	if len(p.pushedDownConds) > 0 {
		conds := make([]expression.Expression, 0, len(p.pushedDownConds))
		for _, cond := range p.pushedDownConds {
			conds = append(conds, cond.Clone())
		}
		is.AccessCondition, is.filterCondition, is.accessEqualCount, is.accessInAndEqCount = DetachIndexScanConditions(conds, idx)
		err = BuildIndexRange(sc, is)
		if err != nil {
			return nil, errors.Trace(err)
		}
		rowCount, err = statsTbl.GetRowCountByIndexRanges(sc, is.Index.ID, is.Ranges, is.accessInAndEqCount)
		if err != nil {
			return nil, errors.Trace(err)
		}
	} else {
		rb := rangeBuilder{sc: sc}
		is.Ranges = rb.buildIndexRanges(fullRange, types.NewFieldType(mysql.TypeNull))
	}
	copTask := &copTaskProfile{
		cnt:       rowCount,
		cst:       rowCount * scanFactor,
		indexPlan: is,
	}
	if !isCoveringIndex(is.Columns, is.Index.Columns, is.Table.PKIsHandle) {
		// On this way, it's double read case.
		copTask.tablePlan = PhysicalTableScan{Columns: p.Columns, Table: is.Table}.init(p.allocator, p.ctx)
		copTask.tablePlan.SetSchema(p.schema)
		var indexCols []*expression.Column
		for _, col := range idx.Columns {
			indexCols = append(indexCols, &expression.Column{FromID: p.id, Position: col.Offset})
		}
		// TODO: We should add PK column to index schema.
		copTask.indexPlan.SetSchema(expression.NewSchema(indexCols...))
	} else {
		is.SetSchema(p.schema)
	}
	// Check if this plan matches the property.
	matchProperty := true
	if !prop.isEmpty() {
		// FIXME: We have not considered the case of index columns with length.
		for i, col := range idx.Columns {
			// not matched
			if col.Name.L == prop.cols[0].ColName.L {
				matchProperty = matchIndicesProp(idx.Columns[i:], prop.cols)
				break
			} else if i >= is.accessEqualCount {
				matchProperty = false
				break
			}
		}
	}
	if matchProperty && !prop.isEmpty() {
		if prop.desc {
			is.Desc = true
			copTask.cst = rowCount * descScanFactor
		}
		task = copTask
	} else {
		is.OutOfOrder = true
		task = prop.enforceProperty(copTask, p.ctx, p.allocator)
	}
	return task, nil
}

func matchIndicesProp(idxCols []*model.IndexColumn, propCols []*expression.Column) bool {
	if len(idxCols) < len(propCols) {
		return false
	}
	for i, col := range propCols {
		if col.ColName.L != propCols[i].ColName.L {
			return false
		}
	}
	return true
}

// convertToTableScan converts the DataSource to table scan.
func (p *DataSource) convertToTableScan(prop *requiredProp) (task taskProfile, err error) {
	ts := PhysicalTableScan{
		Table:       p.tableInfo,
		Columns:     p.Columns,
		TableAsName: p.TableAsName,
		DBName:      p.DBName,
	}.init(p.allocator, p.ctx)
	ts.SetSchema(p.schema)
	sc := p.ctx.GetSessionVars().StmtCtx
	if len(p.pushedDownConds) > 0 {
		conds := make([]expression.Expression, 0, len(p.pushedDownConds))
		for _, cond := range p.pushedDownConds {
			conds = append(conds, cond.Clone())
		}
		ts.AccessCondition, ts.filterCondition = DetachTableScanConditions(conds, p.tableInfo)
		ts.Ranges, err = BuildTableRange(ts.AccessCondition, sc)
		if err != nil {
			return nil, errors.Trace(err)
		}
	} else {
		ts.Ranges = []types.IntColumnRange{{math.MinInt64, math.MaxInt64}}
	}
	statsTbl := p.statisticTable
	rowCount := float64(statsTbl.Count)
	var pkCol *expression.Column
	if p.tableInfo.PKIsHandle {
		for i, colInfo := range ts.Columns {
			if mysql.HasPriKeyFlag(colInfo.Flag) {
				pkCol = p.Schema().Columns[i]
				break
			}
		}
		if pkCol != nil {
			rowCount, err = statsTbl.GetRowCountByIntColumnRanges(sc, pkCol.ID, ts.Ranges)
			if err != nil {
				return nil, errors.Trace(err)
			}
		}
	}
	cost := rowCount * scanFactor
	copTask := &copTaskProfile{
		cnt:               rowCount,
		tablePlan:         ts,
		cst:               cost,
		indexPlanFinished: true,
	}
	task = copTask
	if pkCol != nil && len(prop.cols) == 1 && prop.cols[0].Equal(pkCol, nil) {
		if prop.desc {
			ts.Desc = true
			copTask.cst = rowCount * descScanFactor
		}
		ts.KeepOrder = true
	} else {
		task = prop.enforceProperty(task, p.ctx, p.allocator)
	}
	return task, nil
}
