// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package descs

import (
	"context"

	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/typedesc"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlerrors"
	"github.com/cockroachdb/errors"
)

// GetObjectDesc looks up an object by name and returns both its
// descriptor and that of its parent database. If the object is not
// found and flags.required is true, an error is returned, otherwise
// a nil reference is returned.
//
// TODO(ajwerner): clarify the purpose of the transaction here. It's used in
// some cases for some lookups but not in others. For example, if a mutable
// descriptor is requested, it will be utilized however if an immutable
// descriptor is requested then it will only be used for its timestamp and to
// set the deadline.
func (tc *Collection) GetObjectDesc(
	ctx context.Context, txn *kv.Txn, db, schema, object string, flags tree.ObjectLookupFlags,
) (prefix catalog.ResolvedObjectPrefix, desc catalog.Descriptor, err error) {
	return tc.getObjectByName(ctx, txn, db, schema, object, flags)
}

func (tc *Collection) getObjectByName(
	ctx context.Context,
	txn *kv.Txn,
	catalogName, schemaName, objectName string,
	flags tree.ObjectLookupFlags,
) (prefix catalog.ResolvedObjectPrefix, desc catalog.Descriptor, err error) {
	defer func() {
		if err != nil || desc != nil || !flags.Required {
			return
		}
		if catalogName != "" && prefix.Database == nil {
			err = sqlerrors.NewUndefinedDatabaseError(catalogName)
		} else if prefix.Schema == nil {
			err = sqlerrors.NewUndefinedSchemaError(schemaName)
		} else {
			tn := tree.MakeTableNameWithSchema(
				tree.Name(catalogName),
				tree.Name(schemaName),
				tree.Name(objectName))
			err = sqlerrors.NewUndefinedRelationError(&tn)
		}
	}()
	const alwaysLookupLeasedPublicSchema = false
	prefix, desc, err = tc.getObjectByNameIgnoringRequiredAndType(
		ctx, txn, catalogName, schemaName, objectName, flags,
		alwaysLookupLeasedPublicSchema,
	)
	if err != nil || desc == nil {
		return prefix, nil, err
	}
	if desc.Adding() && desc.IsUncommittedVersion() &&
		(flags.RequireMutable || flags.CommonLookupFlags.AvoidLeased) {
		// Special case: We always return tables in the adding state if they were
		// created in the same transaction and a descriptor (effectively) read in
		// the same transaction is requested. What this basically amounts to is
		// resolving adding descriptors only for DDLs (etc.).
		// TODO (lucy): I'm not sure where this logic should live. We could add an
		// IncludeAdding flag and pull the special case handling up into the
		// callers. Figure that out after we clean up the name resolution layers
		// and it becomes more Clear what the callers should be.
		// TODO(ajwerner): What's weird about returning here is that we have
		// not hydrated the descriptor. I guess the assumption is that it is
		// already hydrated.
		return prefix, desc, nil
	}
	if dropped, err := filterDescriptorState(
		desc, flags.Required, flags.CommonLookupFlags,
	); err != nil || dropped {
		return prefix, nil, err
	}
	switch t := desc.(type) {
	case catalog.TableDescriptor:
		// A given table name can resolve to either a type descriptor or a table
		// descriptor, because every table descriptor also defines an implicit
		// record type with the same name as the table. Thus, depending on the
		// requested descriptor type, we return either the table descriptor itself,
		// or the table descriptor's implicit record type.
		switch flags.DesiredObjectKind {
		case tree.TableObject, tree.TypeObject:
		default:
			return prefix, nil, nil
		}
		tableDesc, err := tc.hydrateTypesInTableDesc(ctx, txn, t)
		if err != nil {
			return prefix, nil, err
		}
		desc = tableDesc
		if flags.DesiredObjectKind == tree.TypeObject {
			// Since a type descriptor was requested, we need to return the implicitly
			// created record type for the table that we found.
			if flags.RequireMutable {
				// ... but, we can't do it if we need a mutable descriptor - we don't
				// have the capability of returning a mutable type descriptor for a
				// table's implicit record type.
				return prefix, nil, pgerror.Newf(pgcode.InsufficientPrivilege,
					"cannot modify table record type %q", objectName)
			}
			desc, err = typedesc.CreateImplicitRecordTypeFromTableDesc(tableDesc)
			if err != nil {
				return prefix, nil, err
			}
		}
	case catalog.TypeDescriptor:
		if flags.DesiredObjectKind != tree.TypeObject {
			return prefix, nil, nil
		}
	default:
		return prefix, nil, errors.AssertionFailedf(
			"unexpected object of type %T", t,
		)
	}
	return prefix, desc, nil
}

func (tc *Collection) getObjectByNameIgnoringRequiredAndType(
	ctx context.Context,
	txn *kv.Txn,
	catalogName, schemaName, objectName string,
	flags tree.ObjectLookupFlags,
	alwaysLookupLeasedPublicSchema bool,
) (prefix catalog.ResolvedObjectPrefix, _ catalog.Descriptor, err error) {

	flags.Required = false
	// If we're reading the object descriptor from the store,
	// we should read its parents from the store too to ensure
	// that subsequent name resolution finds the latest name
	// in the face of a concurrent rename.
	avoidLeasedForParent := flags.AvoidLeased || flags.RequireMutable
	// Resolve the database.
	parentFlags := tree.DatabaseLookupFlags{
		Required:       flags.Required,
		AvoidLeased:    avoidLeasedForParent,
		IncludeDropped: flags.IncludeDropped,
		IncludeOffline: flags.IncludeOffline,
	}

	var db catalog.DatabaseDescriptor
	if catalogName != "" {
		db, err = tc.GetImmutableDatabaseByName(ctx, txn, catalogName, parentFlags)
		if err != nil || db == nil {
			return catalog.ResolvedObjectPrefix{}, nil, err
		}
	}

	prefix.Database = db

	{
		isVirtual, virtualObject, err := tc.virtual.getObjectByName(
			schemaName, objectName, flags, catalogName,
		)
		if err != nil {
			return prefix, nil, err
		}
		if isVirtual {
			sc := tc.virtual.getSchemaByName(schemaName)
			return catalog.ResolvedObjectPrefix{
				Database: db,
				Schema:   sc,
			}, virtualObject, nil
		}
	}

	if catalogName == "" {
		return catalog.ResolvedObjectPrefix{}, nil, nil
	}

	// Read the ID of the schema out of the database descriptor
	// to avoid the need to go look up the schema.
	sc, err := tc.getSchemaByNameMaybeLookingUpPublicSchema(
		ctx, txn, db, schemaName, parentFlags, alwaysLookupLeasedPublicSchema,
	)
	if err != nil || sc == nil {
		return prefix, nil, err
	}

	prefix.Schema = sc
	found, obj, err := tc.getByName(
		ctx, txn, db, sc, objectName, flags.AvoidLeased, flags.RequireMutable, flags.AvoidSynthetic,
		false, // alwaysLookupLeasedPublicSchema
	)
	if !found || err != nil {
		return prefix, nil, err
	}
	return prefix, obj, nil
}
