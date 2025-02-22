// Copyright 2022 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package scmutationexec

import (
	"context"
	"sort"

	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/tabledesc"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scop"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/util/log/eventpb"
	"github.com/cockroachdb/cockroach/pkg/util/protoutil"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/redact"
)

func (m *visitor) MakeAddedColumnDeleteOnly(
	ctx context.Context, op scop.MakeAddedColumnDeleteOnly,
) error {
	col := &descpb.ColumnDescriptor{
		ID:                      op.Column.ColumnID,
		Name:                    tabledesc.ColumnNamePlaceholder(op.Column.ColumnID),
		Hidden:                  op.Column.IsHidden,
		Inaccessible:            op.Column.IsInaccessible,
		GeneratedAsIdentityType: op.Column.GeneratedAsIdentityType,
		PGAttributeNum:          op.Column.PgAttributeNum,
	}
	if o := op.Column.GeneratedAsIdentitySequenceOption; o != "" {
		col.GeneratedAsIdentitySequenceOption = &o
	}
	tbl, err := m.checkOutTable(ctx, op.Column.TableID)
	if err != nil {
		return err
	}
	if col.ID >= tbl.NextColumnID {
		tbl.NextColumnID = col.ID + 1
	}
	return enqueueAddColumnMutation(tbl, col)
}

func (m *visitor) SetAddedColumnType(ctx context.Context, op scop.SetAddedColumnType) error {
	tbl, err := m.checkOutTable(ctx, op.ColumnType.TableID)
	if err != nil {
		return err
	}
	mut, err := FindMutation(tbl, MakeColumnIDMutationSelector(op.ColumnType.ColumnID))
	if err != nil {
		return err
	}
	col := mut.AsColumn().ColumnDesc()
	col.Type = op.ColumnType.Type
	col.Nullable = op.ColumnType.IsNullable
	col.Virtual = op.ColumnType.IsVirtual
	if ce := op.ColumnType.ComputeExpr; ce != nil {
		expr := string(ce.Expr)
		col.ComputeExpr = &expr
		col.UsesSequenceIds = ce.UsesSequenceIDs
	}
	if col.ComputeExpr == nil || !col.Virtual {
		for i := range tbl.Families {
			fam := &tbl.Families[i]
			if fam.ID == op.ColumnType.FamilyID {
				fam.ColumnIDs = append(fam.ColumnIDs, col.ID)
				fam.ColumnNames = append(fam.ColumnNames, col.Name)
				break
			}
		}
	}
	// Empty names are allowed for families, in which case AllocateIDs will assign
	// one.
	return tbl.AllocateIDsWithoutValidation(ctx)
}

func (m *visitor) MakeAddedColumnDeleteAndWriteOnly(
	ctx context.Context, op scop.MakeAddedColumnDeleteAndWriteOnly,
) error {
	tbl, err := m.checkOutTable(ctx, op.TableID)
	if err != nil {
		return err
	}
	return mutationStateChange(
		tbl,
		MakeColumnIDMutationSelector(op.ColumnID),
		descpb.DescriptorMutation_DELETE_ONLY,
		descpb.DescriptorMutation_DELETE_AND_WRITE_ONLY,
		descpb.DescriptorMutation_ADD,
	)
}

func (m *visitor) MakeColumnPublic(ctx context.Context, op scop.MakeColumnPublic) error {
	tbl, err := m.checkOutTable(ctx, op.TableID)
	if err != nil {
		return err
	}
	mut, err := m.removeMutation(tbl, MakeColumnIDMutationSelector(op.ColumnID), op.TargetMetadata, eventpb.CommonSQLEventDetails{
		DescriptorID:    uint32(tbl.GetID()),
		Statement:       redact.RedactableString(op.Statement),
		Tag:             op.StatementTag,
		ApplicationName: op.Authorization.AppName,
		User:            op.Authorization.UserName,
	}, descpb.DescriptorMutation_DELETE_AND_WRITE_ONLY)
	if err != nil {
		return err
	}
	// TODO(ajwerner): Should the op just have the column descriptor? What's the
	// type hydration status here? Cloning is going to blow away hydration. Is
	// that okay?
	tbl.Columns = append(tbl.Columns,
		*(protoutil.Clone(mut.GetColumn())).(*descpb.ColumnDescriptor))

	// Ensure that the column is added in the right location. This is important
	// when rolling back dropped columns.
	getID := func(col *descpb.ColumnDescriptor) int {
		if col.PGAttributeNum != 0 {
			return int(col.PGAttributeNum)
		}
		return int(col.ID)
	}
	sort.Slice(tbl.Columns, func(i, j int) bool {
		return getID(&tbl.Columns[i]) < getID(&tbl.Columns[j])
	})
	return nil
}

func (m *visitor) MakeDroppedColumnDeleteAndWriteOnly(
	ctx context.Context, op scop.MakeDroppedColumnDeleteAndWriteOnly,
) error {
	tbl, err := m.checkOutTable(ctx, op.TableID)
	if err != nil {
		return err
	}
	for i, col := range tbl.PublicColumns() {
		if col.GetID() == op.ColumnID {
			desc := col.ColumnDescDeepCopy()
			tbl.Columns = append(tbl.Columns[:i], tbl.Columns[i+1:]...)
			return enqueueDropColumnMutation(tbl, &desc)
		}
	}
	return errors.AssertionFailedf("failed to find column %d in table %q (%d)",
		op.ColumnID, tbl.GetName(), tbl.GetID())
}

func (m *visitor) MakeDroppedColumnDeleteOnly(
	ctx context.Context, op scop.MakeDroppedColumnDeleteOnly,
) error {
	tbl, err := m.checkOutTable(ctx, op.TableID)
	if err != nil {
		return err
	}
	return mutationStateChange(
		tbl,
		MakeColumnIDMutationSelector(op.ColumnID),
		descpb.DescriptorMutation_DELETE_AND_WRITE_ONLY,
		descpb.DescriptorMutation_DELETE_ONLY,
		descpb.DescriptorMutation_DROP,
	)
}

func (m *visitor) RemoveDroppedColumnType(
	ctx context.Context, op scop.RemoveDroppedColumnType,
) error {
	tbl, err := m.checkOutTable(ctx, op.TableID)
	if err != nil || tbl.Dropped() {
		return err
	}
	mut, err := FindMutation(tbl, MakeColumnIDMutationSelector(op.ColumnID))
	if err != nil || mut.AsColumn().IsSystemColumn() {
		return err
	}
	col := mut.AsColumn().ColumnDesc()
	col.Type = types.Any
	if col.IsComputed() {
		// This operation needs to zero the computed column expression to remove
		// any references to sequences and whatnot but it can't simply remove the
		// expression entirely, otherwise in the case of virtual computed columns
		// the column descriptor will then be interpreted as a virtual non-computed
		// column, which doesn't make any sense.
		null := tree.Serialize(tree.DNull)
		col.ComputeExpr = &null
	}
	return nil
}

func (m *visitor) MakeColumnAbsent(ctx context.Context, op scop.MakeColumnAbsent) error {
	tbl, err := m.checkOutTable(ctx, op.TableID)
	if err != nil || tbl.Dropped() {
		return err
	}
	mut, err := m.removeMutation(tbl, MakeColumnIDMutationSelector(op.ColumnID), op.TargetMetadata, eventpb.CommonSQLEventDetails{
		DescriptorID:    uint32(tbl.GetID()),
		Statement:       redact.RedactableString(op.Statement),
		Tag:             op.StatementTag,
		ApplicationName: op.Authorization.AppName,
		User:            op.Authorization.UserName,
	}, descpb.DescriptorMutation_DELETE_ONLY)
	if err != nil {
		return err
	}
	col := mut.GetColumn()
	tbl.RemoveColumnFromFamilyAndPrimaryIndex(col.ID)
	return nil
}

func (m *visitor) AddColumnFamily(ctx context.Context, op scop.AddColumnFamily) error {
	tbl, err := m.checkOutTable(ctx, op.TableID)
	if err != nil {
		return err
	}
	family := descpb.ColumnFamilyDescriptor{
		Name: op.Name,
		ID:   op.FamilyID,
	}
	tbl.AddFamily(family)
	if family.ID >= tbl.NextFamilyID {
		tbl.NextFamilyID = family.ID + 1
	}
	return nil
}

func (m *visitor) SetColumnName(ctx context.Context, op scop.SetColumnName) error {
	tbl, err := m.checkOutTable(ctx, op.TableID)
	if err != nil {
		return err
	}
	col, err := tbl.FindColumnWithID(op.ColumnID)
	if err != nil {
		return errors.AssertionFailedf("column %d not found in table %q (%d)", op.ColumnID, tbl.GetName(), tbl.GetID())
	}
	return tabledesc.RenameColumnInTable(tbl, col, tree.Name(op.Name), nil /* isShardColumnRenameable */)
}

func (m *visitor) AddColumnDefaultExpression(
	ctx context.Context, op scop.AddColumnDefaultExpression,
) error {
	tbl, err := m.checkOutTable(ctx, op.Default.TableID)
	if err != nil {
		return err
	}
	col, err := tbl.FindColumnWithID(op.Default.ColumnID)
	if err != nil {
		return err
	}
	d := col.ColumnDesc()
	expr := string(op.Default.Expr)
	d.DefaultExpr = &expr
	refs := catalog.MakeDescriptorIDSet(d.UsesSequenceIds...)
	for _, seqID := range op.Default.UsesSequenceIDs {
		if refs.Contains(seqID) {
			continue
		}
		d.UsesSequenceIds = append(d.UsesSequenceIds, seqID)
		refs.Add(seqID)
	}
	return nil
}

func (m *visitor) RemoveColumnDefaultExpression(
	ctx context.Context, op scop.RemoveColumnDefaultExpression,
) error {
	tbl, err := m.checkOutTable(ctx, op.TableID)
	if err != nil || tbl.Dropped() {
		return err
	}
	col, err := tbl.FindColumnWithID(op.ColumnID)
	if err != nil {
		return err
	}
	d := col.ColumnDesc()
	d.DefaultExpr = nil
	return updateColumnExprSequenceUsage(d)
}

func (m *visitor) AddColumnOnUpdateExpression(
	ctx context.Context, op scop.AddColumnOnUpdateExpression,
) error {
	tbl, err := m.checkOutTable(ctx, op.OnUpdate.TableID)
	if err != nil {
		return err
	}
	col, err := tbl.FindColumnWithID(op.OnUpdate.ColumnID)
	if err != nil {
		return err
	}
	d := col.ColumnDesc()
	expr := string(op.OnUpdate.Expr)
	d.OnUpdateExpr = &expr
	refs := catalog.MakeDescriptorIDSet(d.UsesSequenceIds...)
	for _, seqID := range op.OnUpdate.UsesSequenceIDs {
		if refs.Contains(seqID) {
			continue
		}
		d.UsesSequenceIds = append(d.UsesSequenceIds, seqID)
		refs.Add(seqID)
	}
	return nil
}

func (m *visitor) RemoveColumnOnUpdateExpression(
	ctx context.Context, op scop.RemoveColumnOnUpdateExpression,
) error {
	tbl, err := m.checkOutTable(ctx, op.TableID)
	if err != nil || tbl.Dropped() {
		return err
	}
	col, err := tbl.FindColumnWithID(op.ColumnID)
	if err != nil {
		return err
	}
	d := col.ColumnDesc()
	d.OnUpdateExpr = nil
	return updateColumnExprSequenceUsage(d)
}
