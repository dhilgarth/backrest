package api

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"slices"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/garethgeorge/backrest/gen/go/types"
	v1 "github.com/garethgeorge/backrest/gen/go/v1"
	"github.com/garethgeorge/backrest/internal/config"
	"github.com/garethgeorge/backrest/internal/oplog"
	"github.com/garethgeorge/backrest/internal/orchestrator"
	"github.com/garethgeorge/backrest/internal/resticinstaller"
	"github.com/garethgeorge/backrest/internal/rotatinglog"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/proto"
)

func TestUpdateConfig(t *testing.T) {
	sut := createSystemUnderTest(t, &config.MemoryStore{
		Config: &v1.Config{
			Modno: 1234,
		},
	})

	tests := []struct {
		name    string
		req     *v1.Config
		wantErr bool
		res     *v1.Config
	}{
		{
			name: "bad modno",
			req: &v1.Config{
				Modno: 4321,
			},
			wantErr: true,
		},
		{
			name: "good modno",
			req: &v1.Config{
				Modno: 1234,
			},
			wantErr: false,
			res: &v1.Config{
				Modno: 1235,
			},
		},
		{
			name: "reject when validation fails",
			req: &v1.Config{
				Modno: 1235,
				Repos: []*v1.Repo{
					{},
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := sut.handler.SetConfig(context.Background(), connect.NewRequest(tt.req))
			if (err != nil) != tt.wantErr {
				t.Errorf("SetConfig() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if err == nil && !proto.Equal(res.Msg, tt.res) {
				t.Errorf("SetConfig() got = %v, want %v", res, tt.res)
			}
		})
	}
}

func TestBackup(t *testing.T) {
	sut := createSystemUnderTest(t, &config.MemoryStore{
		Config: &v1.Config{
			Modno: 1234,
			Repos: []*v1.Repo{
				{
					Id:       "local",
					Uri:      t.TempDir(),
					Password: "test",
				},
			},
			Plans: []*v1.Plan{
				{
					Id:   "test",
					Repo: "local",
					Paths: []string{
						t.TempDir(),
					},
					Cron: "0 0 1 1 *",
					Retention: &v1.RetentionPolicy{
						KeepHourly: 1,
					},
				},
			},
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		sut.orch.Run(ctx)
	}()

	_, err := sut.handler.Backup(context.Background(), connect.NewRequest(&types.StringValue{Value: "test"}))
	if err != nil {
		t.Fatalf("Backup() error = %v", err)
	}

	// Check that there is a successful backup recorded in the log.
	if slices.IndexFunc(getOperations(t, sut.oplog), func(op *v1.Operation) bool {
		_, ok := op.GetOp().(*v1.Operation_OperationBackup)
		return op.Status == v1.OperationStatus_STATUS_SUCCESS && ok
	}) == -1 {
		t.Fatalf("Expected a backup operation")
	}

	// Wait for the index snapshot operation to appear in the oplog.
	var snapshotId string
	if err := retry(t, 10, 2*time.Second, func() error {
		operations := getOperations(t, sut.oplog)
		if index := slices.IndexFunc(operations, func(op *v1.Operation) bool {
			_, ok := op.GetOp().(*v1.Operation_OperationIndexSnapshot)
			return op.Status == v1.OperationStatus_STATUS_SUCCESS && ok
		}); index != -1 {
			op := operations[index]
			snapshotId = op.SnapshotId
			return nil
		}
		return errors.New("snapshot not indexed")
	}); err != nil {
		t.Fatalf("Couldn't find snapshot in oplog")
	}

	if snapshotId == "" {
		t.Fatalf("snapshotId must be set")
	}

	// Wait for a forget operation to appear in the oplog.
	if err := retry(t, 10, 2*time.Second, func() error {
		operations := getOperations(t, sut.oplog)
		if index := slices.IndexFunc(operations, func(op *v1.Operation) bool {
			_, ok := op.GetOp().(*v1.Operation_OperationForget)
			return op.Status == v1.OperationStatus_STATUS_SUCCESS && ok
		}); index != -1 {
			op := operations[index]
			if op.SnapshotId != snapshotId {
				t.Fatalf("Snapshot ID mismatch on forget operation")
			}
			return nil
		}
		return errors.New("forget not indexed")
	}); err != nil {
		t.Fatalf("Couldn't find forget in oplog")
	}
}

func TestMultipleBackup(t *testing.T) {
	sut := createSystemUnderTest(t, &config.MemoryStore{
		Config: &v1.Config{
			Modno: 1234,
			Repos: []*v1.Repo{
				{
					Id:       "local",
					Uri:      t.TempDir(),
					Password: "test",
				},
			},
			Plans: []*v1.Plan{
				{
					Id:   "test",
					Repo: "local",
					Paths: []string{
						t.TempDir(),
					},
					Cron: "0 0 1 1 *",
					Retention: &v1.RetentionPolicy{
						KeepLastN: 1,
					},
				},
			},
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		sut.orch.Run(ctx)
	}()

	for i := 0; i < 2; i++ {
		_, err := sut.handler.Backup(context.Background(), connect.NewRequest(&types.StringValue{Value: "test"}))
		if err != nil {
			t.Fatalf("Backup() error = %v", err)
		}
	}

	// Wait for a forget that removed 1 snapshot to appear in the oplog
	if err := retry(t, 10, 2*time.Second, func() error {
		operations := getOperations(t, sut.oplog)
		if index := slices.IndexFunc(operations, func(op *v1.Operation) bool {
			forget, ok := op.GetOp().(*v1.Operation_OperationForget)
			return op.Status == v1.OperationStatus_STATUS_SUCCESS && ok && len(forget.OperationForget.Forget) == 1
		}); index != -1 {
			return nil
		}
		return errors.New("forget not indexed")
	}); err != nil {
		t.Fatalf("Couldn't find forget with 1 item removed in the operation log")
	}
}

func TestHookExecution(t *testing.T) {
	dir := t.TempDir()

	hookOutputBefore := path.Join(dir, "before.txt")
	hookOutputAfter := path.Join(dir, "after.txt")

	sut := createSystemUnderTest(t, &config.MemoryStore{
		Config: &v1.Config{
			Modno: 1234,
			Repos: []*v1.Repo{
				{
					Id:       "local",
					Uri:      t.TempDir(),
					Password: "test",
				},
			},
			Plans: []*v1.Plan{
				{
					Id:   "test",
					Repo: "local",
					Paths: []string{
						t.TempDir(),
					},
					Cron: "0 0 1 1 *",
					Hooks: []*v1.Hook{
						{
							Conditions: []v1.Hook_Condition{
								v1.Hook_CONDITION_SNAPSHOT_START,
							},
							Action: &v1.Hook_ActionCommand{
								ActionCommand: &v1.Hook_Command{
									Command: "echo before > " + hookOutputBefore,
								},
							},
						},
						{
							Conditions: []v1.Hook_Condition{
								v1.Hook_CONDITION_SNAPSHOT_END,
							},
							Action: &v1.Hook_ActionCommand{
								ActionCommand: &v1.Hook_Command{
									Command: "echo after > " + hookOutputAfter,
								},
							},
						},
					},
				},
			},
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		sut.orch.Run(ctx)
	}()

	_, err := sut.handler.Backup(context.Background(), connect.NewRequest(&types.StringValue{Value: "test"}))
	if err != nil {
		t.Fatalf("Backup() error = %v", err)
	}

	// Wait for two hook operations to appear in the oplog
	if err := retry(t, 10, 2*time.Second, func() error {
		hookOps := slices.DeleteFunc(getOperations(t, sut.oplog), func(op *v1.Operation) bool {
			_, ok := op.GetOp().(*v1.Operation_OperationRunHook)
			return !ok
		})

		if len(hookOps) == 2 {
			return nil
		}
		return fmt.Errorf("expected 2 hook operations, got %d", len(hookOps))
	}); err != nil {
		t.Fatalf("Couldn't find hooks in oplog: %v", err)
	}

	// expect the hook output files to exist
	if _, err := os.Stat(hookOutputBefore); err != nil {
		t.Fatalf("hook output file before not found")
	}

	if _, err := os.Stat(hookOutputAfter); err != nil {
		t.Fatalf("hook output file after not found")
	}
}

func TestCancelBackup(t *testing.T) {
	sut := createSystemUnderTest(t, &config.MemoryStore{
		Config: &v1.Config{
			Modno: 1234,
			Repos: []*v1.Repo{
				{
					Id:       "local",
					Uri:      t.TempDir(),
					Password: "test",
				},
			},
			Plans: []*v1.Plan{
				{
					Id:   "test",
					Repo: "local",
					Paths: []string{
						t.TempDir(),
					},
					Cron: "0 0 1 1 *",
					Retention: &v1.RetentionPolicy{
						KeepHourly: 1,
					},
				},
			},
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		sut.orch.Run(ctx)
	}()

	// Start a backup
	var errgroup errgroup.Group
	errgroup.Go(func() error {
		backupReq := connect.NewRequest(&types.StringValue{Value: "test"})
		_, err := sut.handler.Backup(context.Background(), backupReq)
		if err != nil {
			return fmt.Errorf("Backup() error = %v", err)
		}
		return nil
	})

	// Find the backup operation ID in the oplog
	var backupOpId int64
	if err := retry(t, 100, 50*time.Millisecond, func() error {
		operations := getOperations(t, sut.oplog)
		for _, op := range operations {
			_, ok := op.GetOp().(*v1.Operation_OperationBackup)
			if ok {
				backupOpId = op.Id
				return nil
			}
		}
		return errors.New("backup operation not found")
	}); err != nil {
		t.Fatalf("Couldn't find backup operation in oplog")
	}

	errgroup.Go(func() error {
		if _, err := sut.handler.Cancel(context.Background(), connect.NewRequest(&types.Int64Value{Value: backupOpId})); err != nil {
			return fmt.Errorf("Cancel() error = %v, wantErr nil", err)
		}
		return nil
	})
	if err := errgroup.Wait(); err != nil {
		t.Fatalf(err.Error())
	}

	// Assert that the backup operation was cancelled
	if slices.IndexFunc(getOperations(t, sut.oplog), func(op *v1.Operation) bool {
		_, ok := op.GetOp().(*v1.Operation_OperationBackup)
		return op.Status == v1.OperationStatus_STATUS_USER_CANCELLED && ok
	}) == -1 {
		t.Fatalf("Expected a cancelled backup operation in the log")
	}
}

type systemUnderTest struct {
	handler  *BackrestHandler
	oplog    *oplog.OpLog
	orch     *orchestrator.Orchestrator
	logStore *rotatinglog.RotatingLog
	config   *v1.Config
}

func createSystemUnderTest(t *testing.T, config config.ConfigStore) systemUnderTest {
	dir := t.TempDir()

	cfg, err := config.Get()
	if err != nil {
		t.Fatalf("Failed to get config: %v", err)
	}

	resticBin, err := resticinstaller.FindOrInstallResticBinary()
	if err != nil {
		t.Fatalf("Failed to find or install restic binary: %v", err)
	}
	oplog, err := oplog.NewOpLog(dir + "/oplog.boltdb")
	if err != nil {
		t.Fatalf("Failed to create oplog: %v", err)
	}
	logStore := rotatinglog.NewRotatingLog(dir+"/log", 10)
	orch, err := orchestrator.NewOrchestrator(
		resticBin, cfg, oplog, logStore,
	)
	if err != nil {
		t.Fatalf("Failed to create orchestrator: %v", err)
	}

	h := NewBackrestHandler(config, orch, oplog, logStore)

	return systemUnderTest{
		handler:  h,
		oplog:    oplog,
		orch:     orch,
		logStore: logStore,
		config:   cfg,
	}
}

func retry(t *testing.T, times int, backoff time.Duration, f func() error) error {
	t.Helper()
	var err error
	for i := 0; i < times; i++ {
		err = f()
		if err == nil {
			return nil
		}
		time.Sleep(backoff)
	}
	return err
}

func getOperations(t *testing.T, oplog *oplog.OpLog) []*v1.Operation {
	t.Logf("Reading oplog")
	operations := []*v1.Operation{}
	if err := oplog.ForAll(func(op *v1.Operation) error {
		operations = append(operations, op)
		t.Logf("operation %t status %s", op.GetOp(), op.Status)
		return nil
	}); err != nil {
		t.Fatalf("Failed to read oplog: %v", err)
	}
	return operations
}
