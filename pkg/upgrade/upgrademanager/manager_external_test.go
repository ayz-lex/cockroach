// Copyright 2020 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package upgrademanager_test

import (
	"context"
	gosql "database/sql"
	"fmt"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/clusterversion"
	"github.com/cockroachdb/cockroach/pkg/jobs"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/batcheval"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/liveness"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/server"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlutil"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/sqlutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/testcluster"
	"github.com/cockroachdb/cockroach/pkg/upgrade"
	"github.com/cockroachdb/cockroach/pkg/upgrade/upgrades"
	"github.com/cockroachdb/cockroach/pkg/util"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/tracing"
	"github.com/cockroachdb/cockroach/pkg/util/tracing/tracingpb"
	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
)

// TestAlreadyRunningJobsAreHandledProperly is a relatively low-level test to
// ensure that the behavior to detect running jobs is sane. The test intercepts
// and blocks an upgrade that it first runs. It then duplicates the job to
// break the single-running job invariant. It then ensures that that invariant
// violation is detected. After that errant job is finished, it ensures that
// concurrent attempts to bump the cluster version detect the already running
// upgrade and wait.
func TestAlreadyRunningJobsAreHandledProperly(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	// We're going to be migrating from startCV to endCV.
	startCV := clusterversion.ClusterVersion{Version: roachpb.Version{Major: 41}}
	endCV := clusterversion.ClusterVersion{Version: roachpb.Version{Major: 42}}

	ch := make(chan chan error)

	ctx := context.Background()
	tc := testcluster.StartTestCluster(t, 2, base.TestClusterArgs{
		ReplicationMode: base.ReplicationManual,
		ServerArgs: base.TestServerArgs{
			Settings: cluster.MakeTestingClusterSettingsWithVersions(endCV.Version, startCV.Version, false),
			Knobs: base.TestingKnobs{
				Server: &server.TestingKnobs{
					BinaryVersionOverride:          startCV.Version,
					DisableAutomaticVersionUpgrade: make(chan struct{}),
				},
				SQLExecutor: &sql.ExecutorTestingKnobs{
					// See the TODO below for why we need this.
					NoStatsCollectionWithVerboseTracing: true,
				},
				UpgradeManager: &upgrade.TestingKnobs{
					ListBetweenOverride: func(from, to clusterversion.ClusterVersion) []clusterversion.ClusterVersion {
						return []clusterversion.ClusterVersion{to}
					},
					RegistryOverride: func(cv clusterversion.ClusterVersion) (upgrade.Upgrade, bool) {
						if cv != endCV {
							return nil, false
						}
						return upgrade.NewTenantUpgrade("test", cv, upgrades.NoPrecondition, func(
							ctx context.Context, version clusterversion.ClusterVersion, deps upgrade.TenantDeps, _ *jobs.Job,
						) error {
							canResume := make(chan error)
							select {
							case ch <- canResume:
							case <-ctx.Done():
								return ctx.Err()
							}
							select {
							case err := <-canResume:
								return err
							case <-ctx.Done():
								return ctx.Err()
							}
						}), true
					},
				},
			},
		},
	})
	defer tc.Stopper().Stop(ctx)

	upgrade1Err := make(chan error, 1)
	go func() {
		_, err := tc.ServerConn(0).ExecContext(ctx, `SET CLUSTER SETTING version = $1`, endCV.String())
		upgrade1Err <- err
	}()
	unblock := <-ch

	// Inject a second job for the same upgrade and ensure that that causes
	// an error. This is pretty gnarly.
	var secondID jobspb.JobID
	require.NoError(t, tc.ServerConn(0).QueryRow(`
   INSERT
     INTO system.jobs
          (
            SELECT
                    unique_rowid(),
                    status,
                    created,
                    payload,
                    progress,
                    created_by_type,
                    created_by_id,
                    claim_session_id,
                    claim_instance_id
              FROM system.jobs
             WHERE (
                    crdb_internal.pb_to_json(
                        'cockroach.sql.jobs.jobspb.Payload',
                        payload
                    )->'migration'
                   ) IS NOT NULL
          )
RETURNING id;`).Scan(&secondID))

	// Make sure that the second job gets run in a timely manner.
	runErr := make(chan error)
	go func() {
		runErr <- tc.Server(0).JobRegistry().(*jobs.Registry).
			Run(
				ctx,
				tc.Server(0).InternalExecutor().(sqlutil.InternalExecutor),
				[]jobspb.JobID{secondID},
			)
	}()
	fakeJobBlockChan := <-ch

	// Ensure that we see the assertion error.
	_, err := tc.Conns[0].ExecContext(ctx, `SET CLUSTER SETTING version = $1`, endCV.String())
	require.Regexp(t, "found multiple non-terminal jobs for version", err)

	// Let the fake, erroneous job finish with an error.
	fakeJobBlockChan <- jobs.MarkAsPermanentJobError(errors.New("boom"))
	require.Regexp(t, "boom", <-runErr)

	// See the TODO below for why we need this.
	_, err = tc.Conns[0].ExecContext(ctx, `SET CLUSTER SETTING sql.txn_stats.sample_rate = 0`)
	require.NoError(t, err)

	// Launch a second upgrade which later we'll ensure does not kick off
	// another job. We'll make sure this happens by polling the trace to see
	// the log line indicating what we want.
	tr := tc.Server(0).TracerI().(*tracing.Tracer)
	recCtx, sp := tr.StartSpanCtx(ctx, "test", tracing.WithRecording(tracingpb.RecordingVerbose))
	defer sp.Finish()
	upgrade2Err := make(chan error, 1)
	go func() {
		// Use an internal executor to get access to the trace as it happens.
		_, err := tc.Server(0).InternalExecutor().(sqlutil.InternalExecutor).Exec(
			recCtx, "test", nil /* txn */, `SET CLUSTER SETTING version = $1`, endCV.String())
		upgrade2Err <- err
	}()

	testutils.SucceedsSoon(t, func() error {
		// TODO(yuzefovich): this check is quite unfortunate since it relies on the
		// assumption that all recordings from the child spans are imported into the
		// tracer. However, this is not the case for the DistSQL processors whose
		// recordings require explicit importing. This only happens when the
		// execution flow is drained which cannot happen until we close the
		// 'unblock' channel, and this we cannot do until we see the expected
		// message in the trace.
		//
		// At the moment it works in a very fragile manner (by making sure that
		// no processors actually create their own spans). In particular, we
		// make sure that the execution statistics are not being collected for
		// the statement by:
		// - disabling the stats collection in the presence of the verbose
		// tracing
		// - disabling the sampling altogether.
		//
		// Instead, a different way to observe the status of the upgrade manager
		// should be introduced and should be used here.
		rec := sp.GetConfiguredRecording()
		if tracing.FindMsgInRecording(rec, "found existing migration job") > 0 {
			return nil
		}
		return errors.Errorf("waiting for job to be discovered: %v", rec)
	})
	close(unblock)
	require.NoError(t, <-upgrade1Err)
	require.NoError(t, <-upgrade2Err)
}

func TestMigrateUpdatesReplicaVersion(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	// We're going to be migrating from startCV to endCV.
	startCV := clusterversion.ClusterVersion{Version: roachpb.Version{Major: 41}}
	endCV := clusterversion.ClusterVersion{Version: roachpb.Version{Major: 42}}

	var desc roachpb.RangeDescriptor
	ctx := context.Background()
	tc := testcluster.StartTestCluster(t, 2, base.TestClusterArgs{
		ReplicationMode: base.ReplicationManual,
		ServerArgs: base.TestServerArgs{
			Settings: cluster.MakeTestingClusterSettingsWithVersions(endCV.Version, startCV.Version, false),
			Knobs: base.TestingKnobs{
				Server: &server.TestingKnobs{
					BinaryVersionOverride:          startCV.Version,
					DisableAutomaticVersionUpgrade: make(chan struct{}),
				},
				UpgradeManager: &upgrade.TestingKnobs{
					ListBetweenOverride: func(from, to clusterversion.ClusterVersion) []clusterversion.ClusterVersion {
						return []clusterversion.ClusterVersion{from, to}
					},
					RegistryOverride: func(cv clusterversion.ClusterVersion) (upgrade.Upgrade, bool) {
						if cv != endCV {
							return nil, false
						}
						return upgrade.NewSystemUpgrade("test", cv, func(
							ctx context.Context, version clusterversion.ClusterVersion, d upgrade.SystemDeps, _ *jobs.Job,
						) error {
							return d.DB.Migrate(ctx, desc.StartKey, desc.EndKey, cv.Version)
						}), true
					},
				},
			},
		},
	})
	defer tc.Stopper().Stop(ctx)
	// RegisterKVMigration the below raft upgrade.
	unregisterKVMigration := batcheval.TestingRegisterMigrationInterceptor(endCV.Version, func() {})
	defer unregisterKVMigration()

	// We'll take a specific range, still running at startCV, generate an
	// outgoing snapshot and then suspend it temporarily. We'll then bump the
	// cluster version on all the stores, as part of the upgrade process, and
	// then resume the snapshot process. Seeing as how the snapshot was
	// generated pre-version bump, off of a version of the range that hadn't
	// observed the upgrade corresponding to the latest cluster version, we
	// expect the store to reject it.

	key := tc.ScratchRange(t)
	require.NoError(t, tc.WaitForSplitAndInitialization(key))
	var err error
	desc, err = tc.LookupRange(key)
	require.NoError(t, err)
	rangeID := desc.RangeID

	// Enqueue the replica in the raftsnapshot queue. We use SucceedsSoon
	// because it may take a bit for raft to figure out that we need to be
	// generating a snapshot.
	store := tc.GetFirstStoreFromServer(t, 0)
	repl, err := store.GetReplica(rangeID)
	require.NoError(t, err)

	if got := repl.Version(); got != startCV.Version {
		t.Fatalf("got replica version %s, expected %s", got, startCV.Version)
	}

	// Wait until all nodes have are considered live.
	nl := tc.Server(0).NodeLiveness().(*liveness.NodeLiveness)
	testutils.SucceedsSoon(t, func() error {
		for _, s := range tc.Servers {
			id := s.NodeID()
			live, err := nl.IsLive(id)
			if err != nil {
				return err
			}
			if !live {
				return errors.Newf("n%s not live yet", id)
			}
		}
		return nil
	})

	// Kick off the upgrade process.
	_, err = tc.Conns[0].ExecContext(ctx, `SET CLUSTER SETTING version = $1`, endCV.String())
	require.NoError(t, err)

	if got := repl.Version(); got != endCV.Version {
		t.Fatalf("got replica version %s, expected %s", got, endCV.Version)
	}
}

// TestConcurrentMigrationAttempts ensures that concurrent attempts to run
// upgrades over a number of versions exhibits reasonable behavior. Namely,
// that each upgrade gets run one time and that upgrades do not get run
// again.
func TestConcurrentMigrationAttempts(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	// We're going to be migrating from startKey to endKey. We end up needing
	// to use real versions because the ListBetween uses the keys compiled into
	// the clusterversion package.
	const (
		startMajor = 42
		endMajor   = 48
	)
	migrationRunCounts := make(map[clusterversion.ClusterVersion]int)

	// RegisterKVMigration the upgrades to update the map with run counts.
	// There should definitely not be any concurrency of execution, so the race
	// detector should not fire.
	var versions []clusterversion.ClusterVersion

	for major := int32(startMajor); major <= endMajor; major++ {
		versions = append(versions, clusterversion.ClusterVersion{
			Version: roachpb.Version{
				Major: major,
			},
		})
	}
	ctx := context.Background()
	var active int32 // used to detect races
	tc := testcluster.StartTestCluster(t, 3, base.TestClusterArgs{
		ReplicationMode: base.ReplicationManual,
		ServerArgs: base.TestServerArgs{
			Settings: cluster.MakeTestingClusterSettingsWithVersions(
				versions[len(versions)-1].Version,
				versions[0].Version,
				false,
			),
			Knobs: base.TestingKnobs{
				Server: &server.TestingKnobs{
					BinaryVersionOverride:          versions[0].Version,
					DisableAutomaticVersionUpgrade: make(chan struct{}),
				},
				UpgradeManager: &upgrade.TestingKnobs{
					ListBetweenOverride: func(from, to clusterversion.ClusterVersion) []clusterversion.ClusterVersion {
						return versions
					},
					RegistryOverride: func(cv clusterversion.ClusterVersion) (upgrade.Upgrade, bool) {
						return upgrade.NewSystemUpgrade("test", cv, func(
							ctx context.Context, version clusterversion.ClusterVersion, d upgrade.SystemDeps, _ *jobs.Job,
						) error {
							if atomic.AddInt32(&active, 1) != 1 {
								t.Error("unexpected concurrency")
							}
							time.Sleep(time.Millisecond)
							atomic.AddInt32(&active, -1)
							migrationRunCounts[version]++
							return nil
						}), true
					},
				},
			},
		},
	})
	defer tc.Stopper().Stop(ctx)

	// Run N instances of the upgrade concurrently on different connections.
	// They should all eventually succeed; some may internally experience a
	// serializable restart but cockroach will handle that transparently.
	// Afterwards we'll ensure that no upgrade was run more than once.
	N := 25
	if util.RaceEnabled {
		N = 5
	}
	db := tc.ServerConn(0)
	db.SetMaxOpenConns(N)
	conns := make([]*gosql.Conn, N)
	for i := range conns {
		var err error
		conns[i], err = db.Conn(ctx)
		require.NoError(t, err)
	}
	var g errgroup.Group
	for i := 0; i < N; i++ {
		conn := conns[i]
		g.Go(func() error {
			_, err := conn.ExecContext(ctx, `SET CLUSTER SETTING version = $1`,
				versions[len(versions)-1].String())
			return err
		})
	}
	require.Nil(t, g.Wait())
	for k, c := range migrationRunCounts {
		require.Equalf(t, 1, c, "version: %v", k)
	}
	require.Len(t, migrationRunCounts, len(versions))
}

// TestPauseMigration ensures that upgrades can indeed be paused and that
// concurrent attempts to perform an upgrade will block on the existing,
// paused job.
func TestPauseMigration(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	// We're going to be migrating from startCV to endCV.
	startCV := clusterversion.ClusterVersion{Version: roachpb.Version{Major: 41}}
	endCV := clusterversion.ClusterVersion{Version: roachpb.Version{Major: 42}}

	type migrationEvent struct {
		unblock  chan<- error
		canceled <-chan struct{}
	}
	ch := make(chan migrationEvent)
	ctx := context.Background()
	tc := testcluster.StartTestCluster(t, 2, base.TestClusterArgs{
		ReplicationMode: base.ReplicationManual,
		ServerArgs: base.TestServerArgs{
			Settings: cluster.MakeTestingClusterSettingsWithVersions(endCV.Version, startCV.Version, false),
			Knobs: base.TestingKnobs{
				JobsTestingKnobs: jobs.NewTestingKnobsWithShortIntervals(),
				Server: &server.TestingKnobs{
					BinaryVersionOverride:          startCV.Version,
					DisableAutomaticVersionUpgrade: make(chan struct{}),
				},
				UpgradeManager: &upgrade.TestingKnobs{
					ListBetweenOverride: func(from, to clusterversion.ClusterVersion) []clusterversion.ClusterVersion {
						return []clusterversion.ClusterVersion{to}
					},
					RegistryOverride: func(cv clusterversion.ClusterVersion) (upgrade.Upgrade, bool) {
						if cv != endCV {
							return nil, false
						}
						return upgrade.NewTenantUpgrade("test", cv, upgrades.NoPrecondition, func(
							ctx context.Context, version clusterversion.ClusterVersion, deps upgrade.TenantDeps, _ *jobs.Job,
						) error {
							canResume := make(chan error)
							ch <- migrationEvent{
								unblock:  canResume,
								canceled: ctx.Done(),
							}
							select {
							case <-ctx.Done():
								return ctx.Err()
							case err := <-canResume:
								return err
							}
						}), true
					},
				},
			},
		},
	})
	defer tc.Stopper().Stop(ctx)

	sqlDB := tc.ServerConn(0)
	upgrade1Err := make(chan error, 1)
	go func() {
		_, err := sqlDB.ExecContext(ctx, `SET CLUSTER SETTING version = $1`, endCV.String())
		upgrade1Err <- err
	}()
	ev := <-ch

	tdb := sqlutils.MakeSQLRunner(sqlDB)
	var id int64
	tdb.QueryRow(t, `
SELECT id
  FROM system.jobs
 WHERE (
        crdb_internal.pb_to_json(
            'cockroach.sql.jobs.jobspb.Payload',
            payload
        )->'migration'
       ) IS NOT NULL;`).
		Scan(&id)
	tdb.Exec(t, "PAUSE JOB $1", id)

	<-ev.canceled
	// Kick off another upgrade
	upgrade2Err := make(chan error, 1)
	go func() {
		_, err := sqlDB.ExecContext(ctx, `SET CLUSTER SETTING version = $1`, endCV.String())
		upgrade2Err <- err
	}()

	// The upgrade should not be done.
	select {
	case err := <-upgrade1Err:
		require.Error(t, err)
		require.Contains(t, err.Error(), "paused before it completed")
	case err := <-upgrade2Err:
		require.Error(t, err)
		require.Contains(t, err.Error(), "paused before it completed")
	case <-ch:
		t.Fatalf("did not expect the job to run again")
	case <-time.After(10 * time.Millisecond):
	}
	// Wait for the job to actually be paused as opposed to waiting in
	// pause-requested. There's a separate issue to make PAUSE wait for
	// the job to be paused, but that's a behavior change is better than nothing.
	tdb.CheckQueryResultsRetry(t, fmt.Sprintf(
		`SELECT status FROM crdb_internal.jobs WHERE job_id = %d`, id,
	), [][]string{{"paused"}})
	tdb.Exec(t, "RESUME JOB $1", id)
	ev = <-ch
	close(ev.unblock)
	_, err := sqlDB.ExecContext(ctx, `SET CLUSTER SETTING version = $1`, endCV.String())
	require.NoError(t, err)
}

// Test that the precondition prevents upgrades from being run.
func TestPrecondition(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	// Start by running v0. We want the precondition of v1 to prevent
	// us from reaching v1 (or v2). We want the precondition to not be
	// run when migrating from v1 to v2.
	next := func(version clusterversion.ClusterVersion) clusterversion.ClusterVersion {
		version.Internal += 2
		return version
	}
	v0 := clusterversion.ClusterVersion{Version: clusterversion.ByKey(clusterversion.V22_1)}
	v1 := next(v0)
	v2 := next(v1)
	versions := []clusterversion.ClusterVersion{v0, v1, v2}
	var migrationRun, preconditionRun int64
	var preconditionErr, migrationErr atomic.Value
	preconditionErr.Store(true)
	migrationErr.Store(true)
	cf := func(run *int64, err *atomic.Value) upgrade.TenantUpgradeFunc {
		return func(
			context.Context, clusterversion.ClusterVersion, upgrade.TenantDeps, *jobs.Job,
		) error {
			atomic.AddInt64(run, 1)
			if err.Load().(bool) {
				return jobs.MarkAsPermanentJobError(errors.New("boom"))
			}
			return nil
		}
	}
	knobs := base.TestingKnobs{
		Server: &server.TestingKnobs{
			DisableAutomaticVersionUpgrade: make(chan struct{}),
			BinaryVersionOverride:          v0.Version,
		},
		// Inject an upgrade which would run to upgrade the cluster.
		// We'll validate that we never create a job for this upgrade.
		UpgradeManager: &upgrade.TestingKnobs{
			ListBetweenOverride: func(from, to clusterversion.ClusterVersion) []clusterversion.ClusterVersion {
				start := sort.Search(len(versions), func(i int) bool { return from.Less(versions[i].Version) })
				end := sort.Search(len(versions), func(i int) bool { return to.Less(versions[i].Version) })
				return versions[start:end]
			},
			RegistryOverride: func(cv clusterversion.ClusterVersion) (upgrade.Upgrade, bool) {
				switch cv {
				case v1:
					return upgrade.NewTenantUpgrade("v1", cv,
						upgrade.PreconditionFunc(func(
							ctx context.Context, cv clusterversion.ClusterVersion, td upgrade.TenantDeps,
						) error {
							return cf(&preconditionRun, &preconditionErr)(ctx, cv, td, nil)
						}),
						cf(&migrationRun, &migrationErr),
					), true
				case v2:
					return upgrade.NewTenantUpgrade("v2", cv,
						upgrades.NoPrecondition,
						cf(&migrationRun, &migrationErr),
					), true
				default:
					return nil, false
				}
			},
		},
	}
	ctx := context.Background()
	args := func() base.TestServerArgs {
		return base.TestServerArgs{
			Knobs: knobs,
			Settings: cluster.MakeTestingClusterSettingsWithVersions(
				v2.Version, // binaryVersion
				v0.Version, // binaryMinSupportedVersion
				false,      // initializeVersion
			),
		}
	}
	tc := testcluster.StartTestCluster(t, 3, base.TestClusterArgs{
		ServerArgsPerNode: map[int]base.TestServerArgs{
			0: args(),
			1: args(),
			2: args(),
		},
	})
	checkActiveVersion := func(t *testing.T, exp clusterversion.ClusterVersion) {
		for i := 0; i < tc.NumServers(); i++ {
			got := tc.Server(i).ClusterSettings().Version.ActiveVersion(ctx)
			require.Equalf(t, exp, got, "server %d", i)
		}
	}
	defer tc.Stopper().Stop(ctx)
	sqlDB := tc.ServerConn(0)
	{
		_, err := sqlDB.Exec("SET CLUSTER SETTING version = $1", v2.String())
		require.Regexp(t, "boom", err)
		require.Equal(t, int64(1), atomic.LoadInt64(&preconditionRun))
		require.Equal(t, int64(0), atomic.LoadInt64(&migrationRun))
		checkActiveVersion(t, v0)
	}
	preconditionErr.Store(false)
	{
		_, err := sqlDB.Exec("SET CLUSTER SETTING version = $1", v2.String())
		require.Regexp(t, "boom", err)
		require.Equal(t, int64(2), atomic.LoadInt64(&preconditionRun))
		require.Equal(t, int64(1), atomic.LoadInt64(&migrationRun))
		checkActiveVersion(t, v0)
	}
	migrationErr.Store(false)
	{
		_, err := sqlDB.Exec("SET CLUSTER SETTING version = $1", v1.String())
		require.NoError(t, err)
		require.Equal(t, int64(3), atomic.LoadInt64(&preconditionRun))
		require.Equal(t, int64(2), atomic.LoadInt64(&migrationRun))
		checkActiveVersion(t, v1)
	}
	preconditionErr.Store(true)
	migrationErr.Store(true)
	{
		_, err := sqlDB.Exec("SET CLUSTER SETTING version = $1", v2.String())
		require.Regexp(t, "boom", err)
		// Note that the precondition is no longer run.
		require.Equal(t, int64(3), atomic.LoadInt64(&preconditionRun))
		require.Equal(t, int64(3), atomic.LoadInt64(&migrationRun))
		checkActiveVersion(t, v1)
	}
	migrationErr.Store(false)
	{
		_, err := sqlDB.Exec("SET CLUSTER SETTING version = $1", v2.String())
		require.NoError(t, err)
		require.Equal(t, int64(3), atomic.LoadInt64(&preconditionRun))
		require.Equal(t, int64(4), atomic.LoadInt64(&migrationRun))
		checkActiveVersion(t, v2)
	}
}
